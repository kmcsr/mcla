package main

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"syscall/js"

	"github.com/GlobeMC/mcla"
	"github.com/GlobeMC/mcla/ghdb"
)

type HTTPStatusErr struct {
	URL        string
	StatusCode int
}

func (e *HTTPStatusErr) Error() string {
	return fmt.Sprintf("HTTP status code error: %d when getting %q", e.StatusCode, e.URL)
}

var ghRepoPrefix = "https://raw.githubusercontent.com/kmcsr/mcla-db-dev/main"

// TODO: use https://developer.mozilla.org/en-US/docs/Web/API/IDBFactory

type JsStorageCache struct {
	storage js.Value
	prefix  string

	workMux sync.RWMutex
	working map[string]chan struct{}
}

var _ ghdb.Cache = &JsStorageCache{}

func NewJsStorageCache(storage js.Value, prefix string) *JsStorageCache {
	return &JsStorageCache{
		storage: storage,
		prefix:  prefix,
		working: make(map[string]chan struct{}, 32),
	}
}

func (s *JsStorageCache) Clear() {
	obj := s.storage.Get("length")
	if obj.Type() == js.TypeNumber {
		leng := obj.Int()
		for i := 0; i < leng; i++ {
			key := s.storage.Call("key", i).String()
			if strings.HasPrefix(key, s.prefix) {
				s.storage.Call("removeItem", key)
			}
		}
	} else if keysFn := s.storage.Get("keys"); keysFn.Type() == js.TypeFunction {
		keys, _ := awaitPromise(keysFn.Invoke())
		if keys.InstanceOf(Array) {
			leng := keys.Length()
			for i := 0; i < leng; i++ {
				key := keys.Index(i).String()
				if strings.HasPrefix(key, s.prefix) {
					s.storage.Call("removeItem", key)
				}
			}
		}
	}
}

func (s *JsStorageCache) Get(key string) string {
	s.workMux.RLock()
	ch := s.working[key]
	s.workMux.RUnlock()
	if ch != nil {
		<-ch
	}
	item := s.storage.Call("getItem", s.prefix+key)
	if item.Truthy() {
		res, _ := awaitPromise(item)
		if res.Truthy() {
			return res.String()
		}
	}
	return ""
}

func (s *JsStorageCache) Set(key string, value string) {
	s.storage.Call("setItem", s.prefix+key, value)
}

func (s *JsStorageCache) Remove(key string) {
	s.storage.Call("removeItem", s.prefix+key)
}

func (s *JsStorageCache) GetOrSet(key string, setter func() string) string {
	v := s.Get(key)
	if v == "" {
		s.workMux.Lock()
		if ch := s.working[key]; ch != nil {
			s.workMux.Unlock()
			return s.Get(key)
		}
		done := make(chan struct{}, 0)
		s.working[key] = done
		s.workMux.Unlock()

		v = setter()
		s.Set(key, v)
		close(done)
		s.workMux.Lock()
		delete(s.working, key)
		s.workMux.Unlock()
	}
	return v
}

const appStorageKeyPrefix = "com.github.kmcsr.mcla."

var defaultErrDB = &ghdb.ErrDB{
	Cache: NewJsStorageCache(localStorage, appStorageKeyPrefix),
	Fetch: func(path string) (io.ReadCloser, error) {
		path, err := url.JoinPath(ghRepoPrefix, path)
		if err != nil {
			return nil, err
		}
		res, err := fetch(path)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return nil, &HTTPStatusErr{res.Url, res.StatusCode}
		}
		return res.Body, nil
	},
}

var defaultAnalyzer = mcla.NewAnalyzer(defaultErrDB)
