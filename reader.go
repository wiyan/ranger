package partial

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const blockSize int = 128 * 1024

type HTTPPartialReaderAt struct {
	URL       *url.URL
	Size      int64
	blockSize int
	client    *http.Client
	blocks    map[int][]byte
	mutex     *sync.RWMutex
}

type requestByteRange struct {
	block      int
	start, end int64
}

func (r requestByteRange) String() string {
	return fmt.Sprintf("%d-%d", r.start, r.end)
}

func (r *HTTPPartialReaderAt) readRangeIntoBlock(rng requestByteRange, reader io.Reader) {
	bn := rng.block
	blocklen := (rng.end - rng.start) + 1
	r.blocks[bn] = make([]byte, blocklen)
	io.ReadFull(reader, r.blocks[bn])
}

func (r *HTTPPartialReaderAt) downloadRanges(ranges []requestByteRange) {
	if len(ranges) > 0 {
		rs := make([]string, len(ranges))
		for i, rng := range ranges {
			rs[i] = rng.String()
		}
		rangeString := strings.Join(rs, ",")

		req, _ := http.NewRequest("GET", r.URL.String(), nil)
		req.Header["Range"] = []string{fmt.Sprintf("bytes=%s", rangeString)}

		resp, _ := r.client.Do(req)
		typ, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		defer resp.Body.Close()

		if typ == "multipart/byteranges" {
			multipart := multipart.NewReader(resp.Body, params["boundary"])
			r.mutex.Lock()
			i := 0
			for {
				if part, err := multipart.NextPart(); err == nil {
					r.readRangeIntoBlock(ranges[i], part)
					i++
				} else {
					break
				}
			}
			r.mutex.Unlock()
		} else {
			r.mutex.Lock()
			r.readRangeIntoBlock(ranges[0], resp.Body)
			r.mutex.Unlock()
		}
	}
}

func (r *HTTPPartialReaderAt) ReadAt(p []byte, off int64) (int, error) {
	l := len(p)
	block := int(off / int64(r.blockSize))
	endBlock := int((off + int64(l)) / int64(r.blockSize))
	endBlockOff := (off + int64(l)) % int64(r.blockSize)
	nblocks := endBlock - block
	if endBlockOff > 0 {
		nblocks++
	}

	ranges := make([]requestByteRange, nblocks)
	nreq := 0
	r.mutex.RLock()
	for i := 0; i < nblocks; i++ {
		bn := block + i
		if _, ok := r.blocks[bn]; ok {
			continue
		}
		ranges[i] = requestByteRange{
			bn,
			int64(bn * r.blockSize),
			int64(((bn + 1) * r.blockSize) - 1),
		}
		if ranges[i].end > r.Size {
			ranges[i].end = r.Size
		}

		nreq++
	}
	r.mutex.RUnlock()
	ranges = ranges[:nreq]

	r.downloadRanges(ranges)
	return r.copyRangeToBuffer(p, off)
}

func (r *HTTPPartialReaderAt) copyRangeToBuffer(p []byte, off int64) (int, error) {
	remaining := len(p)
	block := int(off / int64(r.blockSize))
	startOffset := off % int64(r.blockSize)
	ncopied := 0
	r.mutex.RLock()
	for remaining > 0 {
		copylen := r.blockSize
		if copylen > remaining {
			copylen = remaining
		}

		// if we need to copy more bytes than exist in this block
		if startOffset+int64(copylen) > int64(r.blockSize) {
			copylen = int(int64(r.blockSize) - startOffset)
		}

		if _, ok := r.blocks[block]; !ok {
			return 0, errors.New("fu?")
		}
		copy(p[ncopied:ncopied+copylen], r.blocks[block][startOffset:])

		remaining -= copylen
		ncopied += copylen

		block++
		startOffset = 0
	}
	r.mutex.RUnlock()

	return ncopied, nil
}

func NewPartialReaderAt(u *url.URL) (*HTTPPartialReaderAt, error) {
	resp, _ := http.Head(u.String())
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("404")
	}

	return &HTTPPartialReaderAt{
		URL:       u,
		Size:      resp.ContentLength,
		blockSize: blockSize,
		client:    &http.Client{},
		blocks:    make(map[int][]byte),
		mutex:     &sync.RWMutex{},
	}, nil
}

type LoggingReaderAt struct {
	io.ReaderAt
}

func (r *LoggingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	return r.ReaderAt.ReadAt(p, off)
}
