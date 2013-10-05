// Copyright 2013 Martin Schnabel. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package ws implements a workspace for file resources.
package ws

import (
	"fmt"
	"runtime"
	"sort"
	"sync"
)

// Watcher provides and interface for workspace watchers.
type Watcher interface {
	Watch(r *Res) error
	Close() error
}

// Op describes workspace and filesystem operations or events
type Op uint

const (
	Add Op = 1 << iota
	Change
	Remove
	_
	Create
	Modify
	Delete
	_
	WsMask Op = 0x0F
	FsMask Op = 0xF0
)

// Filter checks new resources and returns true if the resources should be flagged as ignored.
// Resources with FlagIgnore remain in the workspace. Ignored directories are not read.
type Filter interface {
	Filter(*Res) bool
}

// Handler handles resource operation events.
type Handler interface {
	Handle(Op, *Res)
}

// Config contains the configuration used to create new workspaces.
type Config struct {
	// CapHint hints the expected peak resource capacity.
	CapHint uint
	// Watcher returns a new watcher given workspace control.
	// Mounting a path results in a snapshot if no Watcher is configured.
	Watcher func(Controller) (Watcher, error)
	// Handler handles events if set.
	Handler Handler
	// Filter filters resources if set.
	Filter Filter

	*Backend
}

func (c *Config) filter(r *Res) bool {
	if c.Filter != nil {
		return c.Filter.Filter(r)
	}
	return false
}

func (c *Config) handle(op Op, r *Res) {
	if c.Handler != nil {
		c.Handler.Handle(op, r)
	}
}

// Controller provides an interface for the watcher to modify the workspace.
type Controller interface {
	Control(op Op, id Id, name string) error
}

// Ws implements a workspace that manages all mounted resources.
type Ws struct {
	sync.RWMutex
	config  Config
	root    *Res
	all     map[Id]*Res
	watcher Watcher
	fs      Backend
}

// New creates a workspace with configuration c.
func New(c Config) *Ws {
	var name string
	if runtime.GOOS != "windows" {
		name = "/"
	}
	w := &Ws{
		config: c,
		root:   &Res{Id: NewId(name), Name: name, Dir: &Dir{Path: name}},
		all:    make(map[Id]*Res, c.CapHint),
		fs:     Filesystem,
	}
	w.all[w.root.Id] = w.root
	if c.Backend != nil {
		w.fs = *c.Backend
	}
	return w
}

// Mount adds the directory tree at path to the workspace.
func (w *Ws) Mount(path string) (*Res, error) {
	isdir, err := w.fs.IsDir(path)
	if err != nil {
		return nil, err
	}
	if !isdir {
		return nil, fmt.Errorf("not a directory")
	}
	r, err := w.mount(path)
	if err != nil {
		return r, err
	}
	if w.config.filter(r) {
		r.Flag |= FlagIgnore
		return r, nil
	}
	r.Lock()
	err = w.read(r, w.config.Filter)
	r.Unlock()
	if err != nil {
		return r, err
	}
	w.Lock()
	defer w.Unlock()
	w.addAllChildren(0, r)
	return r, nil
}

// Res returns the resource with id or nil.
func (w *Ws) Res(id Id) *Res {
	w.RLock()
	defer w.RUnlock()
	return w.all[id]
}

func (w *Ws) mount(path string) (*Res, error) {
	path = w.fs.Clean(path)
	id := NewId(path)
	d, f := w.fs.Split(path)
	w.Lock()
	defer w.Unlock()
	if w.watcher == nil && w.config.Watcher != nil {
		watcher, err := w.config.Watcher((*ctrl)(w))
		if err != nil {
			return nil, err
		}
		w.watcher = watcher
	}
	r, ok := w.all[id]
	if ok {
		return r, fmt.Errorf("duplicate")
	}
	r = &Res{Id: id, Name: f, Flag: FlagDir | FlagMount, Dir: &Dir{Path: path}}
	// add virtual parent
	r.Parent = w.logicalParent(d)
	r.DirPath = r.Parent.Dir.Path
	r.Parent.Children = insert(r.Parent.Children, r)
	w.all[id] = r
	w.config.handle(Add, r)
	return r, nil
}

// Close closes the workspace.
// The workspace and all its resources are invalid afterwards.
func (w *Ws) Close() {
	w.Lock()
	defer w.Unlock()
	if w.watcher != nil {
		w.watcher.Close()
		w.watcher = nil
	}
	// scatter garbage
	for id, r := range w.all {
		r.Lock()
		r.Dir = nil
		r.Unlock()
		delete(w.all, id)
	}
	w.all = nil
	w.root = nil
}
func (w *Ws) logicalParent(path string) *Res {
	parts := w.split(path)
	r := w.root
	dpath := r.Path()
	for i := len(parts) - 1; i >= 0; i-- {
		if c := find(r.Children, parts[i]); c != nil {
			r = c
			continue
		}
		c := &Res{Name: parts[i], Parent: r, Flag: FlagDir | FlagLogical, DirPath: dpath}
		p := dpath + w.fs.Seperator + c.Name
		c.Dir = &Dir{Path: p}
		c.Id = NewId(p)
		r.Children = insert(r.Children, c)
		w.all[c.Id], r = c, c
	}
	return r
}
func (w *Ws) split(path string) []string {
	parts := make([]string, 0, 8)
	dir, file := path, ""
	for dir != "" {
		if i := len(dir) - 1; dir[i] == w.fs.Seperator[0] {
			dir = dir[:i]
		}
		dir, file = w.fs.Split(dir)
		if file != "" {
			parts = append(parts, file)
			continue
		}
		break
	}
	if dir != "" {
		return append(parts, dir)
	}
	return parts
}
func (w *Ws) read(r *Res, filter Filter) error {
	list, err := w.fs.ReadDir(r.Dir.Path)
	if err != nil {
		return err
	}
	children := make([]*Res, 0, len(list))
	for _, fi := range list {
		c, _ := w.newChild(r, fi.Name(), fi.IsDir(), false)
		children = append(children, c)
	}
	sort.Sort(byTypeAndName(children))
	r.Children = children
	for _, c := range children {
		if filter != nil && filter.Filter(c) {
			c.Flag |= FlagIgnore
			continue
		}
		if c.Flag&FlagDir != 0 {
			if err := w.read(c, filter); err != nil {
				fmt.Println(err)
			}
		}
	}
	return nil
}
func (w *Ws) addAllChildren(fsop Op, r *Res) {
	for _, c := range r.Children {
		w.all[c.Id] = c
		w.config.handle(fsop|Add, c)
		if c.Flag&(FlagDir|FlagIgnore) == FlagDir {
			w.addAllChildren(fsop, c)
		}
	}
	if w.watcher != nil {
		err := w.watcher.Watch(r)
		if err != nil {
			fmt.Println(err)
		}
	}
	w.config.handle(fsop|Change, r)
}
