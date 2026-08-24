package main

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const lastfmAPIRoot = "https://ws.audioscrobbler.com/2.0/"

// LastFMConfig is stored under the "lastfm" key in config.json. SessionKey
// is obtained once via the one-time `--lastfm-auth` setup flow (see main.go)
// and then reused indefinitely - it does not expire unless revoked from the
// user's Last.fm account settings.
type LastFMConfig struct {
	Enabled    bool   `json:"enabled"`
	APIKey     string `json:"api_key"`
	APISecret  string `json:"api_secret"`
	SessionKey string `json:"session_key"`
	Username   string `json:"username"`
}

func (c LastFMConfig) ready() bool {
	return c.Enabled && c.APIKey != "" && c.APISecret != "" && c.SessionKey != ""
}

var lastfmHTTPClient = &http.Client{Timeout: 10 * time.Second}

// lastfmSign implements Last.fm's request-signing scheme: concatenate every
// parameter (excluding "format" and "callback") sorted by key as key+value
// with no separators, append the shared secret, then take the MD5 hex
// digest. See https://www.last.fm/api/authspec.
func lastfmSign(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "format" || k == "callback" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(params[k])
	}
	b.WriteString(secret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func lastfmCall(method string, params map[string]string, secret string, httpMethod string) (map[string]interface{}, error) {
	p := make(map[string]string, len(params)+2)
	for k, v := range params {
		p[k] = v
	}
	p["method"] = method
	p["api_sig"] = lastfmSign(p, secret)
	p["format"] = "json"

	form := url.Values{}
	for k, v := range p {
		form.Set(k, v)
	}

	var resp *http.Response
	var err error
	if httpMethod == http.MethodPost {
		resp, err = lastfmHTTPClient.PostForm(lastfmAPIRoot, form)
	} else {
		resp, err = lastfmHTTPClient.Get(lastfmAPIRoot + "?" + form.Encode())
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("last.fm returned an unexpected response: %s", string(body))
	}
	if code, ok := out["error"]; ok {
		msg, _ := out["message"].(string)
		return out, fmt.Errorf("last.fm error %v: %s", code, msg)
	}
	return out, nil
}

// lastfmGetToken is step 1 of the one-time desktop-style auth flow: request
// an unauthorized token that the user then approves in any web browser.
func lastfmGetToken(apiKey, secret string) (string, error) {
	out, err := lastfmCall("auth.getToken", map[string]string{"api_key": apiKey}, secret, http.MethodGet)
	if err != nil {
		return "", err
	}
	tok, _ := out["token"].(string)
	if tok == "" {
		return "", fmt.Errorf("last.fm did not return a token")
	}
	return tok, nil
}

// lastfmAuthorizeURL builds the URL the user opens (on their phone, laptop,
// whatever - it does not need to be the MiSTer itself) to approve the token
// from lastfmGetToken.
func lastfmAuthorizeURL(apiKey, token string) string {
	return fmt.Sprintf("https://www.last.fm/api/auth/?api_key=%s&token=%s", url.QueryEscape(apiKey), url.QueryEscape(token))
}

// lastfmGetSession is step 2: exchange an approved token for a permanent
// session key to store in config.json.
func lastfmGetSession(apiKey, secret, token string) (sessionKey, username string, err error) {
	out, err := lastfmCall("auth.getSession", map[string]string{"api_key": apiKey, "token": token}, secret, http.MethodGet)
	if err != nil {
		return "", "", err
	}
	session, _ := out["session"].(map[string]interface{})
	sessionKey, _ = session["key"].(string)
	username, _ = session["name"].(string)
	if sessionKey == "" {
		return "", "", fmt.Errorf("last.fm did not return a session key")
	}
	return sessionKey, username, nil
}

// pendingScrobble tracks whatever is currently "now playing" so it can be
// retroactively scrobbled once we know it played long enough to qualify, or
// once something else starts playing.
type pendingScrobble struct {
	artist, title, album string
	startedAt            time.Time
	durationHint         float64 // seconds; 0 means unknown (e.g. live radio)
}

var (
	lastfmMu      sync.Mutex
	lastfmPending *pendingScrobble
)

// lastfmTrackStarted should be called whenever a new song begins playing -
// a local file starting, a queue advancing, or a radio station's ICY
// StreamTitle changing to a new song. It scrobbles whatever was previously
// playing (if it played long enough to qualify under Last.fm's rules) and
// sends a Now Playing update for the new one. durationHint is the track
// length in seconds if known, or 0 for radio where it isn't known in
// advance. Safe to call even when Last.fm isn't configured (cfg.ready() ==
// false); it becomes a no-op other than clearing/tracking pending state.
func lastfmTrackStarted(cfg LastFMConfig, artist, title, album string, durationHint float64) {
	if title == "" {
		return
	}
	lastfmMu.Lock()
	prev := lastfmPending
	lastfmPending = &pendingScrobble{artist: artist, title: title, album: album, startedAt: time.Now(), durationHint: durationHint}
	lastfmMu.Unlock()

	if !cfg.ready() {
		return
	}
	if prev != nil {
		go scrobbleIfQualifies(cfg, prev)
	}
	go func() {
		params := map[string]string{
			"api_key": cfg.APIKey,
			"sk":      cfg.SessionKey,
			"track":   title,
			"artist":  nonEmptyOr(artist, "Unknown Artist"),
		}
		if album != "" {
			params["album"] = album
		}
		if _, err := lastfmCall("track.updateNowPlaying", params, cfg.APISecret, http.MethodPost); err != nil {
			log.Printf("last.fm: now playing update failed: %v", err)
		}
	}()
}

// lastfmTrackStopped should be called when playback stops without a new
// track immediately starting (explicit Stop, end of queue with no repeat,
// app shutting down). It gives whatever was last playing a chance to be
// scrobbled instead of being silently dropped.
func lastfmTrackStopped(cfg LastFMConfig) {
	lastfmMu.Lock()
	prev := lastfmPending
	lastfmPending = nil
	lastfmMu.Unlock()
	if prev != nil && cfg.ready() {
		go scrobbleIfQualifies(cfg, prev)
	}
}

func scrobbleIfQualifies(cfg LastFMConfig, s *pendingScrobble) {
	played := time.Since(s.startedAt).Seconds()

	// Last.fm's scrobbling rule: the track must be longer than 30 seconds,
	// and must have been played for at least half its length or 4 minutes,
	// whichever comes first. Radio has no known duration ahead of time
	// (durationHint == 0), so we fall back to just requiring 30s played,
	// which is the spirit of the rule when length is unknown.
	minPlayed := 30.0
	if s.durationHint > 30 {
		half := s.durationHint / 2
		if half < 240 {
			minPlayed = half
		} else {
			minPlayed = 240
		}
	}
	if played < minPlayed {
		return
	}

	params := map[string]string{
		"api_key":   cfg.APIKey,
		"sk":        cfg.SessionKey,
		"track":     s.title,
		"artist":    nonEmptyOr(s.artist, "Unknown Artist"),
		"timestamp": strconv.FormatInt(s.startedAt.Unix(), 10),
	}
	if s.album != "" {
		params["album"] = s.album
	}
	if _, err := lastfmCall("track.scrobble", params, cfg.APISecret, http.MethodPost); err != nil {
		log.Printf("last.fm: scrobble failed for %q: %v", s.title, err)
	}
}

func nonEmptyOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// runLastFMAuthCLI is the one-time setup flow the user runs by hand:
//
//	mister_hifi --lastfm-auth
//
// It walks through Last.fm's desktop-style auth (get a token, have the
// user approve it in any browser, then exchange it for a permanent session
// key) and saves the result into config.json so the running player can
// scrobble without ever touching the user's Last.fm password.
func runLastFMAuthCLI() {
	cfg := loadConfig()
	reader := bufio.NewReader(os.Stdin)

	apiKey := cfg.LastFM.APIKey
	apiSecret := cfg.LastFM.APISecret
	if apiKey == "" {
		fmt.Print("Last.fm API key: ")
		apiKey, _ = reader.ReadString('\n')
		apiKey = strings.TrimSpace(apiKey)
	}
	if apiSecret == "" {
		fmt.Print("Last.fm API secret: ")
		apiSecret, _ = reader.ReadString('\n')
		apiSecret = strings.TrimSpace(apiSecret)
	}
	if apiKey == "" || apiSecret == "" {
		fmt.Println("A Last.fm API key and secret are required. Get them at https://www.last.fm/api/account/create")
		os.Exit(1)
	}

	token, err := lastfmGetToken(apiKey, apiSecret)
	if err != nil {
		fmt.Println("Failed to get a request token:", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("Open this URL in any browser (your phone is fine) and click Allow:")
	fmt.Println()
	fmt.Println("  " + lastfmAuthorizeURL(apiKey, token))
	fmt.Println()
	fmt.Print("Press Enter once you've approved it... ")
	_, _ = reader.ReadString('\n')

	sessionKey, username, err := lastfmGetSession(apiKey, apiSecret, token)
	if err != nil {
		fmt.Println("Failed to get a session key:", err)
		fmt.Println("Make sure you clicked Allow before pressing Enter, then run --lastfm-auth again.")
		os.Exit(1)
	}

	cfg.LastFM = LastFMConfig{
		Enabled:    true,
		APIKey:     apiKey,
		APISecret:  apiSecret,
		SessionKey: sessionKey,
		Username:   username,
	}
	saveConfig(cfg)

	fmt.Println()
	fmt.Printf("Success! Scrobbling to Last.fm as %q is now enabled in config.json.\n", username)
}
