// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build solaris

package fsnotify

// #include <errno.h>
// #include <strings.h>
// #include <unistd.h>
// #include <stdio.h>
// #include <port.h>
// #include <sys/stat.h>
//
// uintptr_t from_file_obj(struct file_obj *obj) {
//   return (uintptr_t)obj;
// }
//
// struct file_obj*to_file_obj(uintptr_t ptr) {
//   return (struct file_obj*)ptr;
// }
//
// struct file_info {
//   uint mode;
// };
import "C"
import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

// Watcher watches a set of files, delivering events to a channel.
type Watcher struct {
	Events chan Event
	Errors chan error

	port C.int // solaris port for underlying FEN system

	mu      sync.Mutex
	watches map[string]*C.struct_file_obj

	done     chan struct{} // Channel for sending a "quit message" to the reader goroutine
	doneResp chan struct{} // Channel to respond to Close
}

// NewWatcher establishes a new watcher with the underlying OS and begins waiting for events.
func NewWatcher() (*Watcher, error) {
	var err error

	w := new(Watcher)
	w.Events = make(chan Event)
	w.Errors = make(chan error)
	w.port, err = C.port_create()
	if err != nil {
		return nil, err
	}
	w.watches = make(map[string]*C.struct_file_obj)
	w.done = make(chan struct{})
	w.doneResp = make(chan struct{})

	go w.readEvents()
	return w, nil
}

// sendEvent attempts to send an event to the user, returning true if the event
// was put in the channel successfully and false if the watcher has been closed.
func (w *Watcher) sendEvent(e Event) (sent bool) {
	select {
	case w.Events <- e:
		return true
	case <-w.done:
		return false
	}
}

// sendError attempts to send an event to the user, returning true if the error
// was put in the channel successfully and false if the watcher has been closed.
func (w *Watcher) sendError(err error) (sent bool) {
	select {
	case w.Errors <- err:
		return true
	case <-w.done:
		return false
	}
}

func (w *Watcher) isClosed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Close removes all watches and closes the events channel.
func (w *Watcher) Close() error {
	if w.isClosed() {
		return nil
	}
	close(w.done)
	C.close(w.port)
	<-w.doneResp
	return nil
}

// Add starts watching the named file or directory (non-recursively).
func (w *Watcher) Add(path string) error {
	if w.isClosed() {
		return errors.New("FEN watcher already closed")
	}
	stat, err := os.Stat(path)
	switch {
	case err != nil:
		return err
	case stat.IsDir():
		return w.handleDirectory(path, stat, w.associateFile)
	default:
		return w.associateFile(path, stat)
	}
}

// Remove stops watching the the named file or directory (non-recursively).
func (w *Watcher) Remove(path string) error {
	if w.isClosed() {
		return errors.New("FEN watcher already closed")
	}
	if !w.watched(path) {
		return fmt.Errorf("can't remove non-existent FEN watch for: %s", path)
	}

	stat, err := os.Stat(path)
	switch {
	case err != nil:
		return err
	case stat.IsDir():
		return w.handleDirectory(path, stat, w.dissociateFile)
	default:
		return w.dissociateFile(path, stat)
	}
}

// readEvents contains the main loop that runs in a goroutine watching for events.
func (w *Watcher) readEvents() {
	// If this function returns, the watcher has been closed and we can
	// close these channels
	defer close(w.doneResp)
	defer close(w.Errors)
	defer close(w.Events)

	for {
		var pevent C.port_event_t
		_, err := C.port_get(w.port, &pevent, nil)
		if err != nil {
			// port_get failed because we called w.Close()
			if w.isClosed() {
				return
			}
			// There was an error not caused by calling w.Close()
			if !w.sendError(err) {
				return
			}
		}

		if pevent.portev_source != C.PORT_SOURCE_FILE {
			// Event from unexpected source received; should never happen.
			if !w.sendError(errors.New("Event from unexpected source received")) {
				return
			}
			continue
		}

		finfo := (*C.struct_file_info)(pevent.portev_user)
		err = w.handleEvent(pevent.portev_object, pevent.portev_events, finfo)
		if err != nil {
			if !w.sendError(err) {
				return
			}
		}
	}
}

func (w *Watcher) handleDirectory(path string, stat os.FileInfo, handler func(string, os.FileInfo) error) error {
	files, err := ioutil.ReadDir(path)
	if err != nil {
		return err
	}

	// Handle all children of the directory.
	for _, finfo := range files {
		err := handler(filepath.Join(path, finfo.Name()), finfo)
		if err != nil {
			return err
		}
	}

	// And finally handle the directory itself.
	return handler(path, stat)
}

func (w *Watcher) handleEvent(obj C.uintptr_t, events C.int, finfo *C.struct_file_info) error {
	fobj := C.to_file_obj(obj)
	path := C.GoString(fobj.fo_name)
	fmode := os.FileMode(finfo.mode)

	var toSend *Event
	reRegister := true

	switch {
	case events&C.FILE_MODIFIED == C.FILE_MODIFIED:
		if fmode.IsDir() {
			if err := w.updateDirectory(path); err != nil {
				return err
			}
		} else {
			toSend = &Event{path, Write}
		}
	case events&C.FILE_ATTRIB == C.FILE_ATTRIB:
		toSend = &Event{path, Chmod}
	case events&C.FILE_DELETE == C.FILE_DELETE:
		w.unwatch(path)
		toSend = &Event{path, Remove}
		reRegister = false
	case events&C.FILE_RENAME_TO == C.FILE_RENAME_TO:
		// We don't report a Rename event for this case, because
		// Rename events are interpreted as referring to the _old_ name
		// of the file, and in this case the event would refer to the
		// new name of the file. This type of rename event is not
		// supported by fsnotify.

		// inotify reports a Remove event in this case, so we simulate
		// this here.
		if w.watched(path) {
			toSend = &Event{path, Remove}
		}
		// Don't keep watching the file that was removed
		w.unwatch(path)
		reRegister = false
	case events&C.FILE_RENAME_FROM == C.FILE_RENAME_FROM:
		toSend = &Event{path, Rename}
		// Don't keep watching the new file name
		w.unwatch(path)
		reRegister = false
	default:
		return errors.New("unknown event received")
	}

	if toSend != nil {
		if !w.sendEvent(*toSend) {
			return nil
		}
	}
	if !reRegister {
		return nil
	}

	// If we get here, it means we've hit an event above that requires us to
	// continue watching the file
	stat, err := os.Stat(path)
	if err != nil {
		return err
	}
	return w.associateFile(path, stat)
}

func (w *Watcher) updateDirectory(path string) error {
	// The directory was modified, so we must find unwatched entites and
	// watch them. If something was removed from the directory, nothing will
	// happen, as everything else should still be watched.
	files, err := ioutil.ReadDir(path)
	if err != nil {
		return err
	}

	for _, finfo := range files {
		path := filepath.Join(path, finfo.Name())
		if w.watched(path) {
			continue
		}

		err := w.associateFile(path, finfo)
		if err != nil {
			if !w.sendError(err) {
				return nil
			}
		}
		if !w.sendEvent(Event{path, Create}) {
			return nil
		}
	}
	return nil
}

func (w *Watcher) associateFile(path string, stat os.FileInfo) error {
	fobj := buildFileObj(path, stat)
	w.watch(path, &fobj)

	var finfo C.struct_file_info
	finfo.mode = C.uint(stat.Mode())

	mode := C.FILE_MODIFIED | C.FILE_ATTRIB | C.FILE_NOFOLLOW

	_, err := C.port_associate(w.port, C.PORT_SOURCE_FILE, C.from_file_obj(&fobj), C.int(mode), unsafe.Pointer(&finfo))
	return err
}

func (w *Watcher) dissociateFile(path string, stat os.FileInfo) error {
	if !w.watched(path) {
		return nil
	}
	fobj := w.unwatch(path)

	_, err := C.port_dissociate(w.port, C.PORT_SOURCE_FILE, C.from_file_obj(fobj))
	return err
}

func buildFileObj(path string, stat os.FileInfo) C.struct_file_obj {
	var fobj C.struct_file_obj
	fobj.fo_name = C.CString(path)
	fobj.fo_atime.tv_sec = C.time_t(stat.Sys().(*syscall.Stat_t).Atim.Sec)
	fobj.fo_atime.tv_nsec = C.long(stat.Sys().(*syscall.Stat_t).Atim.Nsec)

	fobj.fo_mtime.tv_sec = C.time_t(stat.Sys().(*syscall.Stat_t).Mtim.Sec)
	fobj.fo_mtime.tv_nsec = C.long(stat.Sys().(*syscall.Stat_t).Mtim.Nsec)

	fobj.fo_ctime.tv_sec = C.time_t(stat.Sys().(*syscall.Stat_t).Ctim.Sec)
	fobj.fo_ctime.tv_nsec = C.long(stat.Sys().(*syscall.Stat_t).Ctim.Nsec)
	return fobj
}

func (w *Watcher) watched(path string) bool {
	w.mu.Lock()
	_, found := w.watches[path]
	w.mu.Unlock()
	return found
}

func (w *Watcher) unwatch(path string) *C.struct_file_obj {
	w.mu.Lock()
	fobj := w.watches[path]
	delete(w.watches, path)
	w.mu.Unlock()
	return fobj
}

func (w *Watcher) watch(path string, fobj *C.struct_file_obj) {
	w.mu.Lock()
	w.watches[path] = fobj
	w.mu.Unlock()
}
