package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/* static/*
var content embed.FS

// SSE client management
var (
	clients   = make(map[chan string]bool)
	clientMux sync.Mutex
)

// dataPath returns a filepath rooted at the "data" directory.
// It rejects any path that would escape the data directory.
func dataPath(parts ...string) (string, error) {
	joined := filepath.Join(append([]string{"data"}, parts...)...)
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	dataAbs, err := filepath.Abs("data")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, dataAbs+string(filepath.Separator)) && abs != dataAbs {
		return "", fmt.Errorf("path %q escapes data directory", joined)
	}
	return joined, nil
}

type Entry struct {
	ID         string
	Content    string
	Type       string
	Filename   string
	Size       int64
	ModTime    int64
	Expiry     int64
	IsImage    bool
	IsVideo    bool
	IsViewable bool
	FileIcon   string
	LinkName   string
}

// parseLinkLine parses a "name|url" or bare "url" line from links.file.
// Returns name and url. If no name is set, name equals the url.
func parseLinkLine(line string) (name, rawURL string) {
	if idx := strings.Index(line, "|"); idx != -1 {
		name = strings.TrimSpace(line[:idx])
		rawURL = strings.TrimSpace(line[idx+1:])
	} else {
		rawURL = strings.TrimSpace(line)
		name = rawURL
	}
	return
}

// formatLinkLine serialises a name+url pair back to a links.file line.
func formatLinkLine(name, rawURL string) string {
	name = strings.TrimSpace(name)
	rawURL = strings.TrimSpace(rawURL)
	if name == "" || name == rawURL {
		return rawURL
	}
	return name + "|" + rawURL
}

type ExpirationTracker struct {
	Expirations map[string]time.Time `json:"expirations"`
	mu          sync.RWMutex
}

var expirationTracker *ExpirationTracker
var expirationOptions = []string{"Never", "1 hour", "4 hours", "1 day", "Custom"}

func initExpirationTracker() *ExpirationTracker {
	tracker := &ExpirationTracker{
		Expirations: make(map[string]time.Time),
	}
	// Load existing expirations from file
	expirationFile := filepath.Join("data", "expirations.json")
	if _, err := os.Stat(expirationFile); err == nil {
		data, err := os.ReadFile(expirationFile)
		if err == nil {
			var storedTracker ExpirationTracker
			if err := json.Unmarshal(data, &storedTracker); err == nil {
				tracker.Expirations = storedTracker.Expirations
			}
		}
	}
	return tracker
}

func parseCustomDuration(customExpiry string) time.Duration {
	customExpiry = strings.TrimSpace(customExpiry)
	// Regex to match the format like 1h, 30m, 2d, etc.
	re := regexp.MustCompile(`^(\d+)([hmMdwy])$`)
	matches := re.FindStringSubmatch(customExpiry)
	if len(matches) < 3 { // bad value
		return 5 * time.Minute
	}
	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return 5 * time.Minute
	}
	unit := matches[2]
	switch unit {
	case "m": // minutes
		if value < 5 {
			return 5 * time.Minute
		}
		return time.Duration(value) * time.Minute
	case "h": // hours
		return time.Duration(value) * time.Hour
	case "d": // days
		return time.Duration(value) * 24 * time.Hour
	case "w": // weeks
		return time.Duration(value) * 7 * 24 * time.Hour
	case "M": // months
		return time.Duration(value) * 30 * 24 * time.Hour
	case "y": // years
		return time.Duration(value) * 365 * 24 * time.Hour
	default:
		return 5 * time.Minute
	}
}

func (t *ExpirationTracker) SetExpiration(fileID, expiryOption string) {
	t.mu.Lock()
	if expiryOption == "Never" {
		delete(t.Expirations, fileID)
	} else {
		var duration time.Duration
		switch expiryOption {
		case "1 hour":
			duration = 1 * time.Hour
		case "4 hours":
			duration = 4 * time.Hour
		case "1 day":
			duration = 24 * time.Hour
		case "Custom":
			// Should not happen anymore.
			t.mu.Unlock()
			return
		default:
			if len(expiryOption) > 0 {
				duration = parseCustomDuration(expiryOption)
			} else {
				delete(t.Expirations, fileID)
				t.mu.Unlock()
				t.saveToFile()
				return
			}
		}
		t.Expirations[fileID] = time.Now().Add(duration)
	}
	t.mu.Unlock()
	t.saveToFile()
}

func (t *ExpirationTracker) saveToFile() {
	t.mu.RLock()
	data, err := json.MarshalIndent(t, "", "  ")
	t.mu.RUnlock()
	if err != nil {
		log.Printf("Error marshaling expirations: %v", err)
		return
	}
	expirationFile := filepath.Join("data", "expirations.json")
	if err := os.WriteFile(expirationFile, data, 0644); err != nil {
		log.Printf("Error saving expirations: %v", err)
	}
}

// Reload re-reads expirations.json from disk into the existing tracker
// instance under its own lock. This is used instead of replacing the
// package-level expirationTracker pointer outright (as the /api/import
// handler used to do), which was an unsynchronized concurrent write to a
// variable that every request handler reads — a data race if a request
// came in mid-import.
func (t *ExpirationTracker) Reload() {
	reloaded := initExpirationTracker()
	t.mu.Lock()
	t.Expirations = reloaded.Expirations
	t.mu.Unlock()
}

func (t *ExpirationTracker) CleanupExpired() []string {
	t.mu.Lock()
	now := time.Now()
	var expiredFiles []string
	// Find expired files
	for fileID, expiryTime := range t.Expirations {
		if now.After(expiryTime) {
			expiredFiles = append(expiredFiles, fileID)
		}
	}
	// Delete from tracker map while still holding the lock
	for _, fileID := range expiredFiles {
		delete(t.Expirations, fileID)
	}
	t.mu.Unlock()

	// Remove files from disk (outside lock)
	for _, fileID := range expiredFiles {
		p := filepath.Join("data", fileID)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("Error removing expired file %s: %v", fileID, err)
		} else {
			log.Printf("Removed expired file: %s", fileID)
		}
	}
	if len(expiredFiles) > 0 {
		t.saveToFile()
		notifyContentChange()
	}
	return expiredFiles
}

var listenAddress = flag.String("listen", ":8080", "host:port in which the server will listen")

// Placeholder content for notepad files
const mdPlaceholder = `# Welcome to Markdown Notepad

Start typing your markdown here...

## Features

- **Bold** and *italic* text
- [Links](https://example.com)
- Lists (ordered and unordered)
- Code blocks
- And more!

` + "```" + `
function example() {
  console.log("Hello, Markdown!");
}
` + "```"

// filenameSanitizer strips characters that are illegal in filenames across
// Windows/macOS/Linux: / \ : * ? " < > | and control characters. Everything
// else (!, @, #, $, %, ^, &, +, =, ~, etc.) is allowed.
var filenameSanitizer = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]`)

func sanitizeFilename(baseName string) string {
	return filenameSanitizer.ReplaceAllString(strings.TrimSpace(baseName), "-")
}

// generateUniqueFilename returns a sanitized filename under baseDir that did
// not exist at the moment of the check. It is only safe for callers that
// don't need atomicity (e.g. renaming an existing file); for creating new
// files, use createUniqueFile instead, which closes the check-then-create
// race window between two concurrent requests picking the same name.
func generateUniqueFilename(baseDir, baseName string) string {
	sanitizedName := sanitizeFilename(baseName)
	if _, err := os.Stat(filepath.Join(baseDir, sanitizedName)); os.IsNotExist(err) {
		return sanitizedName
	}
	for {
		randChars := fmt.Sprintf("%04d", rand.Intn(10000))
		newName := fmt.Sprintf("%s-%s", randChars, sanitizedName)
		if _, err := os.Stat(filepath.Join(baseDir, newName)); os.IsNotExist(err) {
			return newName
		}
	}
}

// createUniqueFile atomically creates a new, previously-nonexistent file
// under dir based on baseName. Unlike generateUniqueFilename followed by a
// separate os.Create/os.WriteFile, this closes the TOCTOU race where two
// concurrent uploads with the same name (e.g. two clients both accepting
// the default filename) could both pass the existence check and one would
// silently clobber the other. On a name collision it retries with a random
// prefix, same as generateUniqueFilename, but the existence check and the
// creation are a single atomic O_EXCL operation.
func createUniqueFile(dir, baseName string) (name string, f *os.File, err error) {
	sanitizedName := sanitizeFilename(baseName)
	name = sanitizedName
	for attempt := 0; attempt < 100; attempt++ {
		f, err = os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			return name, f, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
		name = fmt.Sprintf("%04d-%s", rand.Intn(10000), sanitizedName)
	}
	return "", nil, fmt.Errorf("could not create a unique file for %q after multiple attempts", baseName)
}

func handleContentUpdates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Buffered by 1 so a single pending "content_updated" notification isn't
	// silently dropped (see notifyContentChange's non-blocking send) if the
	// client's read loop is momentarily busy writing the previous message.
	messageChan := make(chan string, 1)
	clientMux.Lock()
	clients[messageChan] = true
	clientMux.Unlock()

	defer func() {
		clientMux.Lock()
		delete(clients, messageChan)
		clientMux.Unlock()
		close(messageChan)
	}()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	// Send an initial message
	fmt.Fprintf(w, "data: %s\n\n", "connected")
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-messageChan:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			w.(http.Flusher).Flush()
		case <-ticker.C: // send keep-alive msg
			fmt.Fprintf(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

func notifyContentChange() {
	clientMux.Lock()
	defer clientMux.Unlock()
	for client := range clients {
		select {
		case client <- "content_updated":
		default:
		}
	}
}

func main() {
	flag.Parse()

	if err := os.MkdirAll(filepath.Join("data", "files"), 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join("data", "text"), 0755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join("data", "notepad"), 0755); err != nil {
		log.Fatal(err)
	}
	log.Println("Data directory created/reused without errors.")
	createFileIfNotExists("notepad/md.file", mdPlaceholder)
	createFileIfNotExists("links.file", "")

	// Initialize the expiration tracker
	expirationTracker = initExpirationTracker()
	customExpiry := os.Getenv("DEFAULT_EXPIRY")
	if customExpiry != "" {
		switch customExpiry {
		case "1d":
			expirationOptions = []string{"1 day", "Never", "1 hour", "4 hours", "Custom"}
		case "4h":
			expirationOptions = []string{"4 hours", "Never", "1 hour", "1 day", "Custom"}
		case "1h":
			expirationOptions = []string{"1 hour", "Never", "4 hours", "1 day", "Custom"}
		default:
			expirationOptions = append([]string{customExpiry}, expirationOptions...)
		}
	}

	// Goroutine to periodically expire files
	go func() {
		ticker := time.NewTicker(3 * time.Minute) // 3 minutes is sparse enough, load is extremely minimal as the operation is fast (in memory tracker)
		defer ticker.Stop()
		for range ticker.C {
			expirationTracker.CleanupExpired()
		}
	}()

	tmpl := template.Must(template.ParseFS(content, "templates/*.html"))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Clean up expired files on page load
		expirationTracker.CleanupExpired()
		entries := []Entry{}
		// Read text snippets
		textFiles, _ := os.ReadDir(filepath.Join("data", "text"))
		for _, file := range textFiles {
			if file.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join("data", "text", file.Name()))
			if err != nil {
				continue
			}
			info, _ := file.Info()
			var size int64
			var modTime int64
			if info != nil {
				size = info.Size()
				modTime = info.ModTime().Unix()
			}
			entryID := filepath.Join("text", file.Name())
			var expiry int64
			expirationTracker.mu.RLock()
			if t, ok := expirationTracker.Expirations[entryID]; ok {
				expiry = t.Unix()
			}
			expirationTracker.mu.RUnlock()
			entries = append(entries, Entry{
				ID:       entryID,
				Type:     "text",
				Content:  string(data),
				Filename: file.Name(),
				Size:     size,
				ModTime:  modTime,
				Expiry:   expiry,
			})
		}
		// Read files
		files, _ := os.ReadDir(filepath.Join("data", "files"))
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			info, _ := file.Info()
			var size int64
			var modTime int64
			if info != nil {
				size = info.Size()
				modTime = info.ModTime().Unix()
			}
			entryID := filepath.Join("files", file.Name())
			var expiry int64
			expirationTracker.mu.RLock()
			if t, ok := expirationTracker.Expirations[entryID]; ok {
				expiry = t.Unix()
			}
			expirationTracker.mu.RUnlock()
			entries = append(entries, Entry{
				ID:         entryID,
				Type:       "file",
				Filename:   file.Name(),
				Size:       size,
				ModTime:    modTime,
				Expiry:     expiry,
				IsImage:    isImageFile(file.Name()),
				IsVideo:    isVideoFile(file.Name()),
				IsViewable: isViewableFile(file.Name()),
				FileIcon:   fileIcon(file.Name()),
			})
		}
		// Read links
		data, err := os.ReadFile(filepath.Join("data", "links.file"))
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				linkName, linkURL := parseLinkLine(line)
				entries = append(entries, Entry{
					ID:       "link/" + url.PathEscape(linkURL),
					Type:     "link",
					Content:  linkURL,
					Filename: linkURL,
					LinkName: linkName,
				})
			}
		}
		tmpl.ExecuteTemplate(w, "index.html", entries)
	})

	http.HandleFunc("/md", func(w http.ResponseWriter, r *http.Request) {
		tmpl.ExecuteTemplate(w, "md.html", nil)
	})

	// Retrieve custom expiration options
	http.HandleFunc("/getExpiryOptions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expirationOptions)
	})

	// Serve static files from embedded filesystem
	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		log.Fatalf("Failed to create static sub-filesystem: %v", err)
	}
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// staticMIMEs maps root-level static files to their content types.
	staticMIMEs := map[string]string{
		"style.css":     "text/css",
		"manifest.json": "application/json",
		"sw.js":         "application/javascript",
		"md.js":         "application/javascript",
		"favicon.ico":   "image/x-icon",
		"icon-192.png":  "image/png",
		"icon-512.png":  "image/png",
	}
	for name, ct := range staticMIMEs {
		name, ct := name, ct // capture loop vars
		http.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			f, err := staticFS.Open(name)
			if err != nil {
				http.Error(w, name+" not found", http.StatusNotFound)
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", ct)
			io.Copy(w, f)
		})
	}

	// API endpoint to load notepad content
	http.HandleFunc("/notepad/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			filename := strings.TrimPrefix(r.URL.Path, "/notepad/")
			if filename != "md.file" {
				http.Error(w, "Invalid notepad file", http.StatusBadRequest)
				return
			}
			data, err := os.ReadFile(filepath.Join("data", "notepad", filename))
			if err != nil {
				http.Error(w, "Error reading notepad file", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(data)
			return
		case "POST":
			filename := strings.TrimPrefix(r.URL.Path, "/notepad/")
			if filename != "md.file" {
				http.Error(w, "Invalid notepad file", http.StatusBadRequest)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Error reading request body", http.StatusInternalServerError)
				return
			}
			if err = os.WriteFile(filepath.Join("data", "notepad", filename), body, 0644); err != nil {
				http.Error(w, "Error saving notepad file", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Saved"))
			log.Printf("Saved notepad content to %s\n", filename)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	http.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseMultipartForm(100 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		entryType := r.FormValue("type")
		expiryOption := r.FormValue("expiry")
		submitContent := r.FormValue("content")
		name := r.FormValue("name")
		if entryType == "link" {
			// Handle link submission
			if submitContent == "" {
				http.Error(w, "URL content cannot be empty", http.StatusBadRequest)
				return
			}
			u, err := url.ParseRequestURI(submitContent)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "Invalid URL format. Must start with http:// or https://", http.StatusBadRequest)
				return
			}
			linksFilePath := filepath.Join("data", "links.file")
			f, err := os.OpenFile(linksFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer f.Close()
			linkLine := formatLinkLine(name, submitContent)
			if _, err := f.WriteString(linkLine + "\n"); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			log.Printf("Saved link %s\n", submitContent)
		} else {
			// Handle file and text submission
			files := r.MultipartForm.File["file-upload"]
			if len(files) > 0 {
				// File submission
				for _, fileHeader := range files {
					err := func() error {
						file, err := fileHeader.Open()
						if err != nil {
							return err
						}
						defer file.Close()
						fileName := name
						if fileName == "" {
							fileName = fileHeader.Filename
						}
						uniqueFileName, f, err := createUniqueFile("data/files", fileName)
						if err != nil {
							return err
						}
						defer f.Close()
						if _, err := io.Copy(f, file); err != nil {
							return err
						}
						if expiryOption != "Never" {
							fileID := filepath.Join("files", uniqueFileName)
							expirationTracker.SetExpiration(fileID, expiryOption)
						}
						log.Printf("Saved file %s with expiry %s\n", uniqueFileName, expiryOption)
						return nil
					}()
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
				}
			} else if submitContent != "" {
				// Text snippet submission
				filename := name
				if filename == "" {
					filename = time.Now().Format("Jan-02 15-04-05")
				}
				uniqueFileName, f, err := createUniqueFile("data/text", filename)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				_, err = f.WriteString(submitContent)
				f.Close()
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if expiryOption != "Never" {
					fileID := filepath.Join("text", uniqueFileName)
					expirationTracker.SetExpiration(fileID, expiryOption)
				}
				log.Printf("Saved text snippet %s with expiry %s\n", uniqueFileName, expiryOption)
			}
		}
		notifyContentChange()
		// Send success for AJAX
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Success"))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	http.HandleFunc("/rename/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		oldPath := strings.TrimPrefix(r.URL.Path, "/rename/")
		newName := r.FormValue("newname")
		if newName == "" {
			http.Error(w, "New name cannot be empty", http.StatusBadRequest)
			return
		}
		oldFullPath, err := dataPath(oldPath)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		baseDir := filepath.Dir(oldFullPath)
		newName = generateUniqueFilename(baseDir, newName)
		newPath := filepath.Join(baseDir, newName)

		// Rename on disk first. Only update the expiration tracker (and
		// persist it) once we know the rename actually succeeded — doing it
		// the other way around, as before, left the tracker pointing at a
		// path that was never created if the rename failed partway through.
		if err := os.Rename(oldFullPath, newPath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		expirationTracker.mu.Lock()
		expiryTime, hasExpiry := expirationTracker.Expirations[oldPath]
		if hasExpiry {
			delete(expirationTracker.Expirations, oldPath)
			dataAbs, _ := filepath.Abs("data")
			newAbs, _ := filepath.Abs(newPath)
			relNewPath := strings.TrimPrefix(newAbs, dataAbs+string(filepath.Separator))
			relNewPath = filepath.ToSlash(relNewPath)
			expirationTracker.Expirations[relNewPath] = expiryTime
		}
		expirationTracker.mu.Unlock()
		if hasExpiry {
			expirationTracker.saveToFile()
		}
		notifyContentChange()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		log.Printf("Renamed %s to %s\n", oldPath, newName)
	})

	http.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/raw/")
		if !strings.HasPrefix(id, "text/") {
			http.Error(w, "Only text files can be accessed", http.StatusBadRequest)
			return
		}
		filePath, err := dataPath(id)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			http.Error(w, "File not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
	})

	http.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		filename := strings.TrimPrefix(r.URL.Path, "/download/")
		filePath, err := dataPath(filename)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		file, err := os.Open(filePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()

		// Determine content type: try mime package first, then sniff
		ext := strings.ToLower(filepath.Ext(filename))
		contentType := mime.TypeByExtension(ext)
		if contentType == "" {
			buffer := make([]byte, 512)
			n, err := file.Read(buffer)
			if err != nil && err != io.EOF {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			contentType = http.DetectContentType(buffer[:n])
			if _, err = file.Seek(0, io.SeekStart); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		baseFilename := filepath.Base(filename)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", baseFilename))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if _, err = io.Copy(w, file); err != nil {
			log.Printf("Error serving download %s: %v", filename, err)
		}
		log.Printf("Served %s for download\n", filename)
	})

	http.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		filename := strings.TrimPrefix(r.URL.Path, "/view/")
		filePath, err := dataPath(filename)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		ext := strings.ToLower(filepath.Ext(filename))
		if textPreviewExts[ext] {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}

		http.ServeFile(w, r, filePath)
		log.Printf("Served %s for viewing\n", filename)
	})

	http.HandleFunc("/delete/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/delete/")
		// Handle link deletion
		if after, ok := strings.CutPrefix(id, "link/"); ok {
			linkToDelete, _ := url.PathUnescape(after)
			linksFilePath := filepath.Join("data", "links.file")
			data, err := os.ReadFile(linksFilePath)
			if err != nil {
				http.Error(w, "Failed to read links file for deletion", http.StatusInternalServerError)
				return
			}
			lines := strings.Split(string(data), "\n")
			var newLines []string
			var found bool
			for _, line := range lines {
				_, lineURL := parseLinkLine(line)
				if lineURL == strings.TrimSpace(linkToDelete) && !found {
					found = true // Remove only the first occurrence
					continue
				}
				if strings.TrimSpace(line) != "" {
					newLines = append(newLines, line)
				}
			}
			output := strings.Join(newLines, "\n")
			if output != "" {
				output += "\n"
			}
			err = os.WriteFile(linksFilePath, []byte(output), 0644)
			if err != nil {
				http.Error(w, "Failed to write links file after deletion", http.StatusInternalServerError)
				return
			}
			notifyContentChange()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status": "ok"}`))
			log.Printf("Deleted link %s\n", linkToDelete)
			return
		}
		// Handle file and snippet deletion
		filePath, err := dataPath(id)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		if err := os.Remove(filePath); err != nil {
			log.Printf("Failed to delete %s: %v", id, err)
			http.Error(w, "Failed to delete file", http.StatusInternalServerError)
			return
		}
		expirationTracker.mu.Lock()
		delete(expirationTracker.Expirations, id)
		expirationTracker.mu.Unlock()
		expirationTracker.saveToFile()
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`))
		log.Printf("Deleted %s\n", id)
	})

	http.HandleFunc("/edit/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/edit/")
		if !strings.HasPrefix(id, "text/") {
			http.Error(w, "Can only edit text snippets", http.StatusBadRequest)
			return
		}
		filePath, err := dataPath(id)
		if err != nil {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}
		newContent := r.FormValue("content")
		if newContent == "" {
			http.Error(w, "Content cannot be empty", http.StatusBadRequest)
			return
		}
		// Handle optional rename
		newName := strings.TrimSpace(r.FormValue("name"))
		currentName := filepath.Base(filePath)
		if newName != "" && newName != currentName {
			baseDir := filepath.Dir(filePath)
			// Sanitize the new name the same way generateUniqueFilename would,
			// then check for collision against other files (not the current file).
			reg := regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]`)
			sanitized := strings.TrimSpace(reg.ReplaceAllString(newName, "-"))
			if sanitized == "" {
				sanitized = currentName // fall back if sanitization empties the name
			}
			// Check if target already exists and is a different file
			targetPath := filepath.Join(baseDir, sanitized)
			if _, err := os.Stat(targetPath); err == nil {
				// Collision with a different file — reject rather than mangle
				http.Error(w, "A snippet with that name already exists", http.StatusConflict)
				return
			}
			newPath := targetPath
			// Rename on disk first, then transfer the expiration entry only
			// once the rename has actually succeeded (see the same fix in
			// the /rename/ handler for why order matters here).
			if err := os.Rename(filePath, newPath); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			expirationTracker.mu.Lock()
			if expiry, ok := expirationTracker.Expirations[id]; ok {
				delete(expirationTracker.Expirations, id)
				newID := filepath.ToSlash(strings.TrimPrefix(newPath, "data"+string(filepath.Separator)))
				expirationTracker.Expirations[newID] = expiry
			}
			expirationTracker.mu.Unlock()
			expirationTracker.saveToFile()
			filePath = newPath
			log.Printf("Renamed %s to %s during edit\n", id, sanitized)
		}
		if err := os.WriteFile(filePath, []byte(newContent), 0644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		notifyContentChange()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		log.Printf("Edited %s\n", id)
	})

	// Edit a link — replaces the first occurrence of oldurl with newurl in links.file
	http.HandleFunc("/edit-link/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		oldURL := strings.TrimSpace(r.FormValue("oldurl"))
		if oldURL == "" {
			http.Error(w, "Old URL cannot be empty", http.StatusBadRequest)
			return
		}
		newURL := strings.TrimSpace(r.FormValue("newurl"))
		newName := strings.TrimSpace(r.FormValue("newname"))
		if newURL == "" {
			http.Error(w, "New URL cannot be empty", http.StatusBadRequest)
			return
		}
		u, err := url.ParseRequestURI(newURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			http.Error(w, "Invalid URL format. Must start with http:// or https://", http.StatusBadRequest)
			return
		}
		linksFilePath := filepath.Join("data", "links.file")
		data, err := os.ReadFile(linksFilePath)
		if err != nil {
			http.Error(w, "Failed to read links file", http.StatusInternalServerError)
			return
		}
		lines := strings.Split(string(data), "\n")
		var newLines []string
		var found bool
		for _, line := range lines {
			_, lineURL := parseLinkLine(line)
			if lineURL == strings.TrimSpace(oldURL) && !found {
				newLines = append(newLines, formatLinkLine(newName, newURL))
				found = true
			} else if strings.TrimSpace(line) != "" {
				newLines = append(newLines, strings.TrimSpace(line))
			}
		}
		if !found {
			http.Error(w, "Link not found", http.StatusNotFound)
			return
		}
		output := strings.Join(newLines, "\n") + "\n"
		if err := os.WriteFile(linksFilePath, []byte(output), 0644); err != nil {
			http.Error(w, "Failed to save links file", http.StatusInternalServerError)
			return
		}
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
		log.Printf("Edited link %s -> %s (%s)\n", oldURL, newURL, newName)
	})

	// Reorder links
	http.HandleFunc("/api/reorder-links", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var lines []string
		if err := json.NewDecoder(r.Body).Decode(&lines); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		// Validate each entry contains a proper http/https URL
		for _, l := range lines {
			_, rawURL := parseLinkLine(l)
			u, err := url.ParseRequestURI(rawURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "Invalid URL in list: "+rawURL, http.StatusBadRequest)
				return
			}
		}
		output := strings.Join(lines, "\n")
		if output != "" {
			output += "\n"
		}
		if err := os.WriteFile(filepath.Join("data", "links.file"), []byte(output), 0644); err != nil {
			http.Error(w, "Failed to save links", http.StatusInternalServerError)
			return
		}
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
		log.Println("Reordered links")
	})

	// Bulk delete
	http.HandleFunc("/api/bulk-delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var ids []string
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		var errs []string
		for _, id := range ids {
			if after, ok := strings.CutPrefix(id, "link/"); ok {
				// Link deletion
				linkToDelete, _ := url.PathUnescape(after)
				linksFilePath := filepath.Join("data", "links.file")
				data, err := os.ReadFile(linksFilePath)
				if err != nil {
					errs = append(errs, id)
					continue
				}
				lines := strings.Split(string(data), "\n")
				var newLines []string
				var found bool
				for _, line := range lines {
					_, lineURL := parseLinkLine(line)
					if lineURL == strings.TrimSpace(linkToDelete) && !found {
						found = true
						continue
					}
					if strings.TrimSpace(line) != "" {
						newLines = append(newLines, line)
					}
				}
				output := strings.Join(newLines, "\n")
				if output != "" {
					output += "\n"
				}
				os.WriteFile(linksFilePath, []byte(output), 0644)
			} else {
				// File / snippet deletion
				filePath, err := dataPath(id)
				if err != nil {
					errs = append(errs, id)
					continue
				}
				if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
					errs = append(errs, id)
					continue
				}
				expirationTracker.mu.Lock()
				delete(expirationTracker.Expirations, id)
				expirationTracker.mu.Unlock()
			}
		}
		expirationTracker.saveToFile()
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json")
		if len(errs) > 0 {
			w.WriteHeader(http.StatusMultiStatus)
			json.NewEncoder(w).Encode(map[string]any{"status": "partial", "errors": errs})
		} else {
			w.Write([]byte(`{"status":"ok"}`))
		}
		log.Printf("Bulk deleted %d items\n", len(ids)-len(errs))
	})

	// Export all data as a zip
	http.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="lcs-export.zip"`)
		zw := zip.NewWriter(w)
		defer zw.Close()
		filepath.Walk("data", func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			rel := filepath.ToSlash(strings.TrimPrefix(path, "data"+string(filepath.Separator)))
			f, err := zw.Create(rel)
			if err != nil {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			f.Write(data)
			return nil
		})
		log.Println("Exported data as zip")
	})

	// Import data from a zip upload
	http.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseMultipartForm(500 << 20); err != nil {
			http.Error(w, "Failed to parse form", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "No file provided", http.StatusBadRequest)
			return
		}
		defer file.Close()
		// Read zip into memory (limit 500MB)
		buf, err := io.ReadAll(io.LimitReader(file, 500<<20))
		if err != nil {
			http.Error(w, "Failed to read file", http.StatusInternalServerError)
			return
		}
		zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
		if err != nil {
			http.Error(w, "Invalid zip file", http.StatusBadRequest)
			return
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			// Sanitize path — only allow known app data targets under data/
			cleanPath := filepath.Clean(filepath.FromSlash(f.Name))
			if !filepath.IsLocal(cleanPath) || strings.Contains(cleanPath, "..") {
				continue
			}
			cleanName := filepath.ToSlash(cleanPath)
			switch {
			case cleanName == "links.file", cleanName == "expirations.json":
			case strings.HasPrefix(cleanName, "files/"),
				strings.HasPrefix(cleanName, "text/"),
				strings.HasPrefix(cleanName, "notepad/"):
			default:
				continue
			}
			destPath := filepath.Join("data", filepath.FromSlash(cleanName))
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				continue
			}
			data, readErr := io.ReadAll(rc)
			rc.Close()
			if readErr != nil {
				continue
			}
			if err := os.WriteFile(destPath, data, 0644); err != nil {
				continue
			}
		}
		// Reload expiration tracker after import
		expirationTracker.Reload()
		notifyContentChange()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
		log.Println("Imported zip")
	})

	// Start server
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

// Extension sets used to classify uploaded files. Centralised here (instead
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".avif": true, ".svg": true, ".bmp": true, ".ico": true, ".apng": true,
}
var videoExts = map[string]bool{
	".mp4": true, ".webm": true, ".mov": true, ".ogv": true,
}
var audioExts = map[string]bool{
	".mp3": true, ".wav": true, ".flac": true, ".aac": true, ".m4a": true,
	".ogg": true,
}
var textPreviewExts = map[string]bool{
	".txt": true, ".log": true, ".csv": true, ".md": true, ".json": true,
	".xml": true, ".html": true, ".htm": true, ".css": true, ".js": true,
	".ts": true, ".go": true, ".py": true, ".rb": true, ".sh": true,
	".yaml": true, ".yml": true, ".toml": true, ".rs": true, ".c": true,
	".cpp": true, ".h": true, ".java": true, ".kt": true, ".swift": true,
	".php": true, ".cs": true, ".bash": true, ".ps1": true, ".scss": true,
	".sql": true, ".dockerfile": true, ".ini": true, ".conf": true, ".env": true,
}

// isImageFile returns true for common web-displayable image extensions.
func isImageFile(filename string) bool {
	return imageExts[strings.ToLower(filepath.Ext(filename))]
}

// isVideoFile returns true for browser-playable video extensions.
func isVideoFile(filename string) bool {
	return videoExts[strings.ToLower(filepath.Ext(filename))]
}

// isViewableFile returns true for files browsers can display natively.
func isViewableFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return imageExts[ext] || videoExts[ext] || audioExts[ext] || textPreviewExts[ext] || ext == ".pdf"
}

// fileIcon returns a FontAwesome icon class for a given filename extension.
func fileIcon(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch {
	case imageExts[ext]:
		return "fa-file-image"
	case ext == ".pdf":
		return "fa-file-pdf"
	case ext == ".doc" || ext == ".docx":
		return "fa-file-word"
	case ext == ".xls" || ext == ".xlsx":
		return "fa-file-excel"
	case ext == ".ppt" || ext == ".pptx":
		return "fa-file-powerpoint"
	case ext == ".zip" || ext == ".tar" || ext == ".gz" || ext == ".bz2" || ext == ".xz" || ext == ".7z" ||
		ext == ".rar" || ext == ".lz" || ext == ".zst" || ext == ".cab" || ext == ".Z" || ext == ".lzma" ||
		ext == ".lz" || ext == ".lz4" || ext == ".lzo" || ext == ".z":
		return "fa-file-zipper"
	case ext == ".iso" || ext == ".img" || ext == ".ima" || ext == ".dmg" || ext == ".cpio" || ext == ".vhd" ||
		ext == ".vmdk" || ext == ".vdi" || ext == ".dsk" || ext == ".toast" || ext == ".vhdx" || ext == ".disk" ||
		ext == ".qcow" || ext == ".qcow2" || ext == ".hfs" || ext == ".hfv" || ext == ".raw" || ext == ".apfs" ||
		ext == ".cdr":
		return "fa-compact-disc"
	case ext == ".go" || ext == ".py" || ext == ".js" || ext == ".ts" || ext == ".rb" || ext == ".rs" ||
		ext == ".c" || ext == ".cpp" || ext == ".h" || ext == ".java" || ext == ".kt" || ext == ".swift" ||
		ext == ".php" || ext == ".cs" || ext == ".sh" || ext == ".bash" || ext == ".ps1" || ext == ".html" ||
		ext == ".htm" || ext == ".css" || ext == ".scss" || ext == ".json" || ext == ".yaml" || ext == ".yml" ||
		ext == ".toml" || ext == ".xml" || ext == ".sql" || ext == ".md" || ext == ".dockerfile":
		return "fa-file-code"
	case ext == ".txt" || ext == ".log" || ext == ".csv" || ext == ".ini" || ext == ".conf" || ext == ".env":
		return "fa-file-lines"
	case audioExts[ext] || ext == ".aif" || ext == ".wma":
		return "fa-file-audio"
	case videoExts[ext] || ext == ".avi" || ext == ".flv" || ext == ".mpg" || ext == ".mpeg" || ext == ".wmv" ||
		ext == ".mkv":
		return "fa-file-video"
	case ext == ".ttf" || ext == ".otf" || ext == ".woff" || ext == ".woff2":
		return "fa-file-alt"
	default:
		return "fa-file"
	}
}
func createFileIfNotExists(filename string, defaultContent string) {
	dir := filepath.Dir(filepath.Join("data", filename))
	if dir != "." && dir != "data" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("Error creating directory %s: %v\n", dir, err)
		}
	}
	filePath := filepath.Join("data", filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		err := os.WriteFile(filePath, []byte(defaultContent), 0644)
		if err != nil {
			log.Printf("Error creating file %s: %v\n", filename, err)
		} else {
			log.Printf("Created file %s with default content\n", filename)
		}
	}
}
