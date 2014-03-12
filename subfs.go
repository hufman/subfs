package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/mdlayher/gosubsonic"
)

// subsonic stores the instance of the gosubsonic client
var subsonic gosubsonic.Client

// nameToDir maps a directory name to its SubDir
var nameToDir map[string]SubDir

// nameToFile maps a file name to its SubFile
var nameToFile map[string]SubFile

// fileCache maps a file name to its file pointer
var fileCache map[string]os.File

// cacheTotal is the total size of local files in the cache
var cacheTotal int64

// host is the host of the Subsonic server
var host = flag.String("host", "", "Host of Subsonic server")

// user is the username to connect to the Subsonic server
var user = flag.String("user", "", "Username for the Subsonic server")

// password is the password to connect to the Subsonic server
var password = flag.String("password", "", "Password for the Subsonic server")

// mount is the path where subfs will be mounted
var mount = flag.String("mount", "", "Path where subfs will be mounted")

// cacheSize is the maximum size of the local file cache in megabytes
var cacheSize = flag.Int64("cache", 100, "Size of the local file cache, in megabytes")

func main() {
	// Parse command line flags
	flag.Parse()

	// Open connection to Subsonic
	sub, err := gosubsonic.New(*host, *user, *password)
	if err != nil {
		log.Fatalf("Could not connect to Subsonic server: %s", err.Error())
	}

	// Store subsonic client for global use
	subsonic = *sub

	// Initialize lookup maps
	nameToDir = map[string]SubDir{}
	nameToFile = map[string]SubFile{}

	// Initialize file cache
	fileCache = map[string]os.File{}
	cacheTotal = 0

	// Attempt to mount filesystem
	c, err := fuse.Mount(*mount)
	if err != nil {
		log.Fatalf("Could not mount subfs at %s: %s", *mount, err.Error())
	}

	// Serve the FUSE filesystem
	log.Printf("subfs: %s@%s -> %s [cache: %d MB]", *user, *host, *mount, *cacheSize)
	go fs.Serve(c, SubFS{})

	// Wait for termination singals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	signal.Notify(sigChan, syscall.SIGTERM)
	for sig := range sigChan {
		log.Println("subfs: caught signal:", sig)
		break
	}

	// Unmount the FUSE filesystem
	if err := fuse.Unmount(*mount); err != nil {
		log.Fatalf("Could not unmount subfs at %s: %s", *mount, err.Error())
	}

	// Close the FUSE filesystem
	if err := c.Close(); err != nil {
		log.Fatalf("Could not close subfs: %s", err.Error())
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

	log.Printf("subfs: removed %d cached files", len(fileCache))
	return
}

// SubFS represents the root of the filesystem
type SubFS struct{}

// Root is called to get the root directory node of this filesystem
func (fs SubFS) Root() (fs.Node, fuse.Error) {
	return &SubDir{RelPath: ""}, nil
}

// SubDir represents a directory in the filesystem
type SubDir struct {
	ID      int64
	RelPath string
}

// Attr retrives the attributes for this SubDir
func (SubDir) Attr() fuse.Attr {
	return fuse.Attr{
		Mode: os.ModeDir | 0555,
	}
}

// ReadDir returns a list of directory entries depending on the current path
func (d SubDir) ReadDir(intr fs.Intr) ([]fuse.Dirent, fuse.Error) {
	// List of directory entries to return
	directories := make([]fuse.Dirent, 0)

	// If at root of filesystem, fetch indexes
	if d.RelPath == "" {
		index, err := subsonic.GetIndexes(-1, -1)
		if err != nil {
			log.Printf("Failed to retrieve indexes: %s", err.Error())
			return nil, fuse.ENOENT
		}

		// Iterate indices
		for _, i := range index {
			// Iterate all artists
			for _, a := range i.Artist {
				// Map artist's name to directory
				nameToDir[a.Name] = SubDir{
					ID:      a.ID,
					RelPath: "",
				}

				// Create a directory entry
				dir := fuse.Dirent{
					Name: a.Name,
					Type: fuse.DT_Dir,
				}

				// Append entry
				directories = append(directories, dir)
			}
		}
	} else {
		// Get this directory's contents
		content, err := subsonic.GetMusicDirectory(d.ID)
		if err != nil {
			log.Printf("Failed to retrieve directory %d: %s", d.ID, err.Error())
			return nil, fuse.ENOENT
		}

		// Iterate all returned directories
		for _, dir := range content.Directories {
			// Create a directory entry
			entry := fuse.Dirent{
				Name: dir.Title,
				Type: fuse.DT_Dir,
			}

			// Add SubDir directory to lookup map
			nameToDir[dir.Title] = SubDir{
				ID:      dir.ID,
				RelPath: d.RelPath + dir.Title,
			}

			// Append to list
			directories = append(directories, entry)
		}

		// Iterate all returned audio
		for _, a := range content.Audio {
			// Predefined audio filename format
			audioFormat := fmt.Sprintf("%02d - %s - %s.%s", a.Track, a.Artist, a.Title, a.Suffix)

			// Create a directory entry
			dir := fuse.Dirent{
				Name: audioFormat,
				Type: fuse.DT_File,
			}

			// Add SubFile file to lookup map
			nameToFile[dir.Name] = SubFile{
				ID:       a.ID,
				Created:  a.Created,
				FileName: audioFormat,
				Size:     a.Size,
			}

			// Append to list
			directories = append(directories, dir)
		}

		// Iterate all returned video
		for _, v := range content.Video {
			// Predefined video filename format
			videoFormat := fmt.Sprintf("%s.%s", v.Title, v.Suffix)

			// Create a directory entry
			dir := fuse.Dirent{
				Name: videoFormat,
				Type: fuse.DT_File,
			}

			// Add SubFile file to lookup map
			nameToFile[dir.Name] = SubFile{
				ID:       v.ID,
				Created:  v.Created,
				FileName: videoFormat,
				Size:     v.Size,
			}

			// Append to list
			directories = append(directories, dir)
		}
	}

	// Return all directory entries
	return directories, nil
}

// Lookup scans the current directory for matching files or directories
func (d SubDir) Lookup(name string, intr fs.Intr) (fs.Node, fuse.Error) {
	// Lookup directory by name
	if dir, ok := nameToDir[name]; ok {
		dir.RelPath = name + "/"
		return dir, nil
	}

	// Lookup file by name
	if f, ok := nameToFile[name]; ok {
		return f, nil
	}

	// File not found
	return nil, fuse.ENOENT
}

// SubFile represents a file in Subsonic library
type SubFile struct {
	ID       int64
	Created  time.Time
	FileName string
	Size     int64
}

// Attr returns file attributes (all files read-only)
func (s SubFile) Attr() fuse.Attr {
	return fuse.Attr{
		Mode:  0644,
		Mtime: s.Created,
		Size:  uint64(s.Size),
	}
}

// ReadAll opens a file stream from Subsonic and returns the resulting bytes
func (s SubFile) ReadAll(intr fs.Intr) ([]byte, fuse.Error) {
	// Byte stream to return data
	byteChan := make(chan []byte)

	// Fetch file in background
	go func() {
		// Check for file in cache
		if cFile, ok := fileCache[s.FileName]; ok {
			// Check for empty file, meaning the cached file got wiped out
			buf, err := ioutil.ReadFile(cFile.Name())
			if len(buf) == 0 && strings.Contains(err.Error(), "no such file or directory") {
				// Purge item from cache
				log.Printf("Cache missing: [%d] %s", s.ID, s.FileName)
				delete(fileCache, s.FileName)
				cacheTotal = atomic.AddInt64(&cacheTotal, -1 * s.Size)

				// Print some cache metrics
				cacheUse := float64(cacheTotal) / 1024 / 1024
				cacheDel := float64(s.Size) / 1024 / 1024
				log.Printf("Cache use: %0.3f / %d.000 MB (-%0.3f MB)", cacheUse, *cacheSize, cacheDel)

				// Close file handle
				if err := cFile.Close(); err != nil {
					log.Println(err)
				}
			} else {
				// Return cached file
				log.Printf("Cached file: [%d] %s", s.ID, s.FileName)
				byteChan <- buf
				return
			}
		}

		// Open stream
		log.Printf("Opening stream: [%d] %s", s.ID, s.FileName)
		stream, err := subsonic.Stream(s.ID, nil)
		if err != nil {
			log.Println(err)
			byteChan <- nil
			return
		}

		// Read in stream
		file, err := ioutil.ReadAll(stream)
		if err != nil {
			log.Println(err)
			byteChan <- nil
			return
		}

		// Close stream
		if err := stream.Close(); err != nil {
			log.Println(err)
			byteChan <- nil
			return
		}

		// Return bytes
		log.Printf("Closing stream: [%d] %s", s.ID, s.FileName)
		byteChan <- file

		// Check for maximum cache size
		if cacheTotal > *cacheSize*1024*1024 {
			log.Printf("Cache full (%d MB), skipping local cache", *cacheSize)
			return
		}

		// Check if cache will overflow if file is added
		if cacheTotal+s.Size > *cacheSize*1024*1024 {
			log.Printf("File will overflow cache (%0.3f MB), skipping local cache", float64(s.Size)/1024/1024)
			return
		}

		// If file is greater than 50MB, skip caching to conserve memory
		threshold := 50
		if s.Size > int64(threshold*1024*1024) {
			log.Printf("File too large (%0.3f > %0d MB), skipping local cache", float64(s.Size)/1024/1024, threshold)
			return
		}

		// Generate a temporary file
		tmpFile, err := ioutil.TempFile(os.TempDir(), "subfs")
		if err != nil {
			log.Println(err)
			return
		}

		// Write out temporary file
		if _, err := tmpFile.Write(file); err != nil {
			log.Println(err)
			return
		}

		// Add file to cache map
		log.Printf("Caching file: [%d] %s", s.ID, s.FileName)
		fileCache[s.FileName] = *tmpFile

		// Add file's size to cache total size
		cacheTotal = atomic.AddInt64(&cacheTotal, s.Size)

		// Print some cache metrics
		cacheUse := float64(cacheTotal) / 1024 / 1024
		cacheAdd := float64(s.Size) / 1024 / 1024
		log.Printf("Cache use: %0.3f / %d.000 MB (+%0.3f MB)", cacheUse, *cacheSize, cacheAdd)

		return
	}()

	// Wait for an event on read
	select {
	// Byte stream channel
	case stream := <-byteChan:
		return stream, nil
	// Interrupt channel
	case <-intr:
		return nil, fuse.EINTR
	}
}
