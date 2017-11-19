package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/mdlayher/goset"
	"github.com/mdlayher/gosubsonic"
)

// SubDir represents a directory in the filesystem
type SubDir struct {
	ID      int64
	Root    bool
	Folder  bool
	dirs    map[string]SubDir
	files   map[string]SubFile
}

func NewSubDir(ID int64, Root bool, Folder bool) SubDir{
	var newDir = SubDir{
		ID:   ID,
		Root: Root,
		Folder: Folder,
	}
	// contents of directory
	newDir.dirs = map[string]SubDir{}
	newDir.files = map[string]SubFile{}
	return newDir
}

// Attr retrives the attributes for this SubDir
func (SubDir) Attr() fuse.Attr {
	return fuse.Attr{
		Mode: os.ModeDir | 0555,
	}
}

// Create does nothing, because subfs is read-only
func (SubDir) Create(req *fuse.CreateRequest, res *fuse.CreateResponse, intr fs.Intr) (fs.Node, fs.Handle, fuse.Error) {
	return nil, nil, fuse.Errno(syscall.EROFS)
}

// Fsync does nothing, because subfs is read-only
func (SubDir) Fsync(req *fuse.FsyncRequest, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Link does nothing, because subfs is read-only
func (SubDir) Link(req *fuse.LinkRequest, node fs.Node, intr fs.Intr) (fs.Node, fuse.Error) {
	return nil, fuse.Errno(syscall.EROFS)
}

// Lookup scans the current directory for matching files or directories
func (d SubDir) Lookup(name string, intr fs.Intr) (fs.Node, fuse.Error) {
	// If directory hasn't loaded, load things first
	if len(d.dirs) == 0 && len(d.files) == 0 {
		d.ReadDir(intr)
	}

	// Lookup directory by name
	if dir, ok := d.dirs[name]; ok {
		return dir, nil
	}

	// Lookup file by name
	if f, ok := d.files[name]; ok {
		return f, nil
	}

	// File not found
	return nil, fuse.ENOENT
}

// ReadDir returns a list of directory entries depending on the current path
func (d SubDir) ReadDir(intr fs.Intr) ([]fuse.Dirent, fuse.Error) {
	// List of directory entries to return
	directories := make([]fuse.Dirent, 0)

	// If at root of filesystem, fetch indexes
	if d.Root {
		// If empty, wait for indexes to be available
		if len(artistsIndex) == 0 {
			<-indexChan
		}

		// Create the All Entries
		d.dirs["All"] = NewSubDir(
			-1,
			false,
			true,
		)
		// Create a directory entry
		dir := fuse.Dirent{
			Name: "All",
			Type: fuse.DT_Dir,
		}
		directories = append(directories, dir)

		// Iterate through the music folders
		for folder, _ := range artistsIndex {
			d.dirs[folder.Name] = NewSubDir(
				folder.ID,
				false,
				true,
			)
			// Create a directory entry
			dir := fuse.Dirent{
				Name: folder.Name,
				Type: fuse.DT_Dir,
			}

			// Append entry
			directories = append(directories, dir)
		}
		return directories, nil
	}

	// Top level Music Folder
	if d.Folder {
		for folder, artists := range artistsIndex {
			if (d.ID == folder.ID || d.ID == -1) {
				log.Printf("Music Folder name: %s", folder.Name)
				// Iterate all artists
				for _, a := range artists {
					// Map artist's name to directory
					d.dirs[a.Name] = NewSubDir(
						a.ID,
						false,
						false,
					)

					// Create a directory entry
					dir := fuse.Dirent{
						Name: a.Name,
						Type: fuse.DT_Dir,
					}

					// Append entry
					directories = append(directories, dir)
				}
			}
		}

		return directories, nil
	}

	// Not at filesystem root, so get this directory's contents
	content, err := subsonic.GetMusicDirectory(d.ID)
	if err != nil {
		log.Printf("subfs: failed to retrieve directory %d: %s", d.ID, err.Error())
		return nil, fuse.ENOENT
	}

	// Check for unique, available cover art IDs
	coverArt := set.New()

	// List of bad characters which should be replaced in filenames
	badChars := []string{"/", "\\"}

	// Iterate all returned directories
	for _, dir := range content.Directories {
		// Check for any characters which may cause trouble with filesystem display
		for _, b := range badChars {
			dir.Title = strings.Replace(dir.Title, b, "_", -1)
		}

		// Create a directory entry
		entry := fuse.Dirent{
			Name: dir.Title,
			Type: fuse.DT_Dir,
		}

		// Add SubDir directory to lookup map
		d.dirs[dir.Title] = NewSubDir(
			dir.ID,
			false,
			false,
		)

		// Check for cover art
		coverArt.Add(dir.CoverArt)

		// Append to list
		directories = append(directories, entry)
	}

	// Iterate all returned audio
	for _, a := range content.Audio {

		// Check for lossless and lossy transcode
		transcodes := []struct {
			suffix string
			size   int64
		}{
			{a.Suffix, a.Size},
			{a.TranscodedSuffix, 0},
		}

		for _, t := range transcodes {
			// If suffix is empty (source is lossy), skip this file
			if t.suffix == "" {
				continue
			}

			// Mark file as lossless by default
			lossless := true

			// If size is empty (transcode to lossy), estimate it and mark as lossy
			if t.size == 0 {
				lossless = false

				// Since we have no idea what Subsonic's transcoding settings are, we will estimate
				// using MP3 CBR 320 as our benchmark, being that it will likely over-estimate
				// Thanks: http://www.jeffreysward.com/editorials/mp3size.htm
				t.size = ((a.DurationRaw * 320) / 8) * 1024
			}

			// Predefined audio filename format
			var filenameCtx = struct{
				A gosubsonic.Audio
				Artist string
				Album string
				Track int64
				Title string
				Suffix string
				Path string
				Basename string
			}{
				A: a,
				Artist: a.Artist,
				Album: a.Album,
				Track: a.Track,
				Title: a.Title,
				Suffix: a.Suffix,
				Path: a.Path,
				Basename: strings.TrimSuffix(a.Path, t.suffix),
			}

			var filenameBuffer bytes.Buffer
			err := filenameTemplate.Execute(&filenameBuffer, filenameCtx)
			if err != nil {
				log.Printf("subfs: failed to format filename %d: %s", a.Path, err.Error())
				continue
			}
			var filename = filenameBuffer.String()

			// Check for any characters which may cause trouble with filesystem display
			for _, b := range badChars {
				filename = strings.Replace(filename, b, "_", -1)
			}

			// Create a directory entry
			dir := fuse.Dirent{
				Name: filename,
				Type: fuse.DT_File,
			}

			// Add SubFile file to lookup map
			d.files[dir.Name] = SubFile{
				ID:       a.ID,
				Created:  a.Created,
				FileName: filename,
				IsVideo:  false,
				Lossless: lossless,
				Size:     t.size,
			}

			// Check for cover art
			coverArt.Add(a.CoverArt)

			// Append to list
			directories = append(directories, dir)
		}
	}

	// Iterate all returned video
	for _, v := range content.Video {
		// Predefined video filename format
		videoFormat := fmt.Sprintf("%s.%s", v.Title, v.Suffix)

		// Check for any characters which may cause trouble with filesystem display
		for _, b := range badChars {
			videoFormat = strings.Replace(videoFormat, b, "_", -1)
		}

		// Create a directory entry
		dir := fuse.Dirent{
			Name: videoFormat,
			Type: fuse.DT_File,
		}

		// Add SubFile file to lookup map
		d.files[dir.Name] = SubFile{
			ID:       v.ID,
			Created:  v.Created,
			FileName: videoFormat,
			Size:     v.Size,
			IsVideo:  true,
		}

		// Check for cover art
		coverArt.Add(v.CoverArt)

		// Append to list
		directories = append(directories, dir)
	}

	// Iterate all cover art
	for _, e := range coverArt.Enumerate() {
		// Type-hint to int64
		c := e.(int64)
		coverArtFormat := fmt.Sprintf("%d.jpg", c)

		// Create a directory entry
		dir := fuse.Dirent{
			Name: coverArtFormat,
			Type: fuse.DT_File,
		}

		// Add SubFile file to lookup map
		d.files[dir.Name] = SubFile{
			ID:       c,
			FileName: coverArtFormat,
			IsArt:    true,
		}

		// Append to list
		directories = append(directories, dir)
	}

	// Return all directory entries
	return directories, nil
}

// Mkdir does nothing, because subfs is read-only
func (SubDir) Mkdir(req *fuse.MkdirRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	return nil, fuse.Errno(syscall.EROFS)
}

// Mknod does nothing, because subfs is read-only
func (SubDir) Mknod(req *fuse.MknodRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	return nil, fuse.Errno(syscall.EROFS)
}

// Remove does nothing, because subfs is read-only
func (SubDir) Remove(req *fuse.RemoveRequest, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Removexattr does nothing, because subfs is read-only
func (SubDir) Removexattr(req *fuse.RemovexattrRequest, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Rename does nothing, because subfs is read-only
func (SubDir) Rename(req *fuse.RenameRequest, node fs.Node, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Setattr does nothing, because subfs is read-only
func (SubDir) Setattr(req *fuse.SetattrRequest, res *fuse.SetattrResponse, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Setxattr does nothing, because subfs is read-only
func (SubDir) Setxattr(req *fuse.SetxattrRequest, intr fs.Intr) fuse.Error {
	return fuse.Errno(syscall.EROFS)
}

// Symlink does nothing, because subfs is read-only
func (SubDir) Symlink(req *fuse.SymlinkRequest, intr fs.Intr) (fs.Node, fuse.Error) {
	return nil, fuse.Errno(syscall.EROFS)
}
