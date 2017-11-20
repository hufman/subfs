package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"text/template"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/mdlayher/gosubsonic"
)

// subsonic stores the instance of the gosubsonic client
var subsonic gosubsonic.Client

// fileCache maps a file name to its file pointer
var fileCache map[string]os.File

// filenameTemplate describes how to format a filename
var filenameTemplate *template.Template

// cacheTotal is the total size of local files in the cache
var cacheTotal int64

// artistsIndex stores the fetched top-level artists
var artistsIndex map[gosubsonic.MusicFolder][]gosubsonic.IndexArtist

// indexChan blocks subfs from getting indexes until the cache is populated
var indexChan chan bool

// streamMap maps a fileID to a channel containing a file stream
var streamMap map[int64]chan []byte

// cacheSize is the maximum size of the local file cache in megabytes
var cacheSize = flag.Int64("cache", 100, "Size of the local file cache, in megabytes")

// fileCacheSize stores any corrected transcoded filesizes
// we find out the corrected size during a Fuse callback,
// and we don't have a shared reference to a SubFile then
var fileSizeCache map[int64]int64

// helper method for filename templates
// Strips the extension from a Path or Filename
func stripExtension(filename string) string {
	var pieces = strings.Split(filename, ".")
	return strings.Join(pieces[:len(pieces)-1], ".")
}

func main() {
	// Flags to connect to Subsonic server
	host := flag.String("host", "", "Host of Subsonic server")
	user := flag.String("user", "", "Username for the Subsonic server")
	password := flag.String("password", "", "Password for the Subsonic server")

	// Flag for subfs mount point
	mount := flag.String("mount", "", "Path where subfs will be mounted")

	// Flag for filename template
	// Another useful option is:
	// {{if eq .A.TranscodedSuffix ""}}{{.Filename}}{{else}}{{ if eq .Suffix "mp3" }}{{.Basename }}.{{.Suffix}}{{else}}{{end}}{{end}}
	// Which hides the original files from transcodes
	// Alternatively, this one includes the original extension
	// {{if eq .A.TranscodedSuffix ""}}{{.Filename}}{{else}}{{ if eq .Suffix "mp3" }}{{.Filename }}.{{.Suffix}}{{else}}{{end}}{{end}}
	filenameTmpl := flag.String("filenames", "{{printf \"%02d - %s - %s.%s\" .A.Track .A.Artist .A.Title .A.Suffix}}", "Template for filenames")

	// Parse command line flags
	flag.Parse()

	// Open connection to Subsonic
	sub, err := gosubsonic.New(*host, *user, *password)
	if err != nil {
		log.Fatalf("Could not connect to Subsonic server: %s", err.Error())
	}

	// Store subsonic client for global use
	subsonic = *sub

	// Save other parameters
	templateFunctions := make(template.FuncMap)
	templateFunctions["Split"] = strings.Split
	templateFunctions["Title"] = strings.Title
	templateFunctions["ToLower"] = strings.ToLower
	templateFunctions["ToTitle"] = strings.ToTitle
	templateFunctions["ToUpper"] = strings.ToUpper
	templateFunctions["Trim"] = strings.Trim
	templateFunctions["TrimLeft"] = strings.TrimLeft
	templateFunctions["TrimRight"] = strings.TrimRight
	templateFunctions["Base"] = path.Base
	templateFunctions["Dir"] = path.Dir
	templateFunctions["Ext"] = path.Ext
	templateFunctions["stripExt"] = stripExtension
	filenameTemplate, err = template.New("filenameTemplate").Funcs(templateFunctions).Parse(*filenameTmpl)
	if err != nil {
		log.Fatalf("Could not parse filenameTemplate: %s", *filenameTmpl)
	}

	// Initialize file cache
	fileCache = map[string]os.File{}
	cacheTotal = 0

	// Initialize index cache
	artistsIndex = make(map[gosubsonic.MusicFolder][]gosubsonic.IndexArtist)
	indexChan = make(chan bool, 0)
	go cacheIndexes()

	// Initialize the updated filesize cache
	fileSizeCache = make(map[int64]int64)

	// Initialize stream map
	streamMap = map[int64]chan []byte{}

	// Attempt to mount filesystem
	c, err := fuse.Mount(*mount)
	if err != nil {
		log.Fatalf("Could not mount subfs at %s: %s", *mount, err.Error())
	}

	// Serve the FUSE filesystem
	log.Printf("subfs: %s@%s -> %s [cache: %d MB]", *user, *host, *mount, *cacheSize)
	go func() {
		if err := fs.Serve(c, SubFS{}); err != nil {
			log.Fatalf("Could not serve subfs at %s: %s", *mount, err.Error())
		}
	}()

	// Wait for termination singals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	signal.Notify(sigChan, syscall.SIGTERM)
	for sig := range sigChan {
		log.Println("subfs: caught signal:", sig)
		break
	}

	// Purge all cached files
	for _, f := range fileCache {
		// Close file
		if err := f.Close(); err != nil {
			log.Println(err)
		}

		// Remove file
		if err := os.Remove(f.Name()); err != nil {
			log.Println(err)
		}
	}

	log.Printf("subfs: removed %d cached file(s)", len(fileCache))

	// Attempt to unmount the FUSE filesystem
	retry := 3
	for i := 0; i < retry+1; i++ {
		// Wait between attempts
		if i > 0 {
			<-time.After(time.Second * 3)
		}

		// Try unmount
		if err := fuse.Unmount(*mount); err != nil {
			// Force exit on last attempt
			if i == retry {
				log.Printf("subfs: could not unmount %s, halting!", *mount)
				os.Exit(1)
			}

			log.Printf("subfs: could not unmount %s, retrying %d of %d...", *mount, i+1, retry)
		} else {
			break
		}
	}

	// Close the FUSE filesystem
	if err := c.Close(); err != nil {
		log.Fatalf("Could not close subfs: %s", err.Error())
	}

	log.Printf("subfs: done!")
	return
}

// cacheIndexes populates and refills the indexes cache at regular intervals
func cacheIndexes() {
	// Immediately cache the current index
	for {
		// Fetch the main folders
		folders, err := subsonic.GetMusicFolders()
		if err != nil {
			log.Printf("Failed to retrieve music folders: %s", err.Error())
			return
		}

		// Fetch indexes
		for _, folder := range folders {
			// get all the letters of this folder
			indexes, err := subsonic.GetIndexes(folder.ID, -1)
			if err != nil {
				log.Printf("Failed to retrieve indexes: %s", err.Error())
				continue
			}

			// Cache and return indexes
			artistsIndex[folder] = make([]gosubsonic.IndexArtist, 0)
			for _, i := range indexes {
				for _, a := range i.Artist {
					artistsIndex[folder] = append(artistsIndex[folder], a)
				}
			}
			log.Printf("Caching %d artists", len(artistsIndex[folder]))
		}
		log.Printf("Finished caching artists")
		indexChan <- true

		// Repeat at regular intervals
		<-time.After(10 * time.Minute)
	}
}

// SubFS represents the root of the filesystem
type SubFS struct{}

// Root is called to get the root directory node of this filesystem
func (fs SubFS) Root() (fs.Node, fuse.Error) {
	return NewSubDir(-1, true, false), nil
}
