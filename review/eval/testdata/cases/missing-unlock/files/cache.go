package cache

import (
	"errors"
	"sync"
)

var errEmptyKey = errors.New("empty key")

type Cache struct {
	mu    sync.Mutex
	items map[string]string
}

func (c *Cache) Set(key string, value string) error {
	c.mu.Lock()
	if key == "" {
		return errEmptyKey
	}
	c.items[key] = value
	c.mu.Unlock()
	return nil
}
