package main

import (
	"bytes"
	"sync"
)

type execution struct {
	WallMS      int64  `json:"wallMs"`
	PeakRSS     int64  `json:"peakProcessTreeRssBytes"`
	RSSObserved bool   `json:"rssObserved"`
	Exit        int    `json:"exitCode"`
	Failure     string `json:"failure,omitempty"`
	Stdout      string `json:"-"`
	Stderr      string `json:"-"`
}

type limitedBuffer struct {
	mu       sync.Mutex
	b        bytes.Buffer
	exceeded bool
	limit    int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	room := b.limit - b.b.Len()
	if len(p) > room {
		p = p[:room]
		b.exceeded = true
	}
	b.b.Write(p)
	return n, nil
}
func (b *limitedBuffer) value() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String(), b.exceeded
}
