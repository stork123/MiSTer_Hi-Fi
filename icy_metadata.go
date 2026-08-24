package main

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// icyCopy copies audio bytes from src to dst while stripping out the ICY
// metadata blocks that Shoutcast/Icecast servers interleave into the byte
// stream every metaInt bytes once a request is sent with "Icy-MetaData: 1".
// Whenever the embedded StreamTitle changes, onTitle is called with the new,
// already-unescaped title. If metaInt is 0 the server isn't sending
// metadata and this behaves exactly like io.Copy.
//
// preConsumed is how many audio bytes (relative to the very start of the
// HTTP response body) the caller already consumed from src before calling
// icyCopy - e.g. by discarding leading junk to reach the first valid MP3
// frame. It's used to keep the metaInt boundary aligned with what the
// server is actually counting, since ICY metadata offsets are always
// relative to byte 0 of the body, not to wherever icyCopy starts reading.
func icyCopy(dst io.Writer, src *bufio.Reader, metaInt int, preConsumed int, onTitle func(string)) error {
	if metaInt <= 0 {
		_, err := io.Copy(dst, src)
		return err
	}

	buf := make([]byte, 8192)
	lastTitle := ""

	firstChunk := metaInt
	if preConsumed > 0 {
		if off := preConsumed % metaInt; off != 0 {
			firstChunk = metaInt - off
		}
	}

	remaining := firstChunk
	for {
		for remaining > 0 {
			n := len(buf)
			if remaining < n {
				n = remaining
			}
			r, err := src.Read(buf[:n])
			if r > 0 {
				if _, werr := dst.Write(buf[:r]); werr != nil {
					return werr
				}
				remaining -= r
			}
			if err != nil {
				return err
			}
		}

		lenByte, err := src.ReadByte()
		if err != nil {
			return err
		}
		metaLen := int(lenByte) * 16
		if metaLen > 0 {
			meta := make([]byte, metaLen)
			if _, err := io.ReadFull(src, meta); err != nil {
				return err
			}
			if title := parseStreamTitle(meta); title != "" && title != lastTitle {
				lastTitle = title
				if onTitle != nil {
					onTitle(title)
				}
			}
		}

		remaining = metaInt
	}
}

// parseStreamTitle extracts the value of StreamTitle='...' from a raw ICY
// metadata block, e.g. "StreamTitle='Artist - Song';StreamUrl='...';".
func parseStreamTitle(meta []byte) string {
	s := string(meta)
	const key = "StreamTitle='"
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	s = s[i+len(key):]
	j := strings.Index(s, "';")
	if j < 0 {
		j = strings.LastIndex(s, "'")
	}
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(s[:j])
}

// splitArtistTitle best-effort splits the common radio "Artist - Title"
// StreamTitle convention. If no separator is found, artist comes back empty
// and the whole string is returned as the title.
func splitArtistTitle(streamTitle string) (artist, title string) {
	for _, sep := range []string{" - ", " – ", " — "} {
		if idx := strings.Index(streamTitle, sep); idx > 0 {
			return strings.TrimSpace(streamTitle[:idx]), strings.TrimSpace(streamTitle[idx+len(sep):])
		}
	}
	return "", strings.TrimSpace(streamTitle)
}

func parseMetaInt(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
