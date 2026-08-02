package main

import (
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
	ID       string
	Content  string
	Type     string
	Filename string
	Size     int64
	ModTime  int64
	Expiry   int64
	IsImage  bool
	FileIcon string
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
	unit := strings.ToLower(matches[2])
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

func generateUniqueFilename(baseDir, baseName string) string {
	baseName = strings.TrimSpace(baseName)
	// Sanitize: allow only letters (+unicode), numbers, space, dot, hyphen, underscore, () and []
	reg := regexp.MustCompile(`[^\p{L}\p{N}\p{M}\s\.\-_()\[\]]`)
	sanitizedName := reg.ReplaceAllString(baseName, "-")
	log.Printf("Sanitized name %s TO %s\n", baseName, sanitizedName)
	// First try without random prefix
	if _, err := os.Stat(filepath.Join(baseDir, sanitizedName)); os.IsNotExist(err) {
		return sanitizedName
	}
	// If file exists, add random prefix until we find a unique name
	for {
		randChars := fmt.Sprintf("%04d", rand.Intn(10000))
		newName := fmt.Sprintf("%s-%s", randChars, sanitizedName)
		if _, err := os.Stat(filepath.Join(baseDir, newName)); os.IsNotExist(err) {
			return newName
		}
	}
}

func handleContentUpdates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	messageChan := make(chan string)
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
				ID:       entryID,
				Type:     "file",
				Filename: file.Name(),
				Size:     size,
				ModTime:  modTime,
				Expiry:   expiry,
				IsImage:  isImageFile(file.Name()),
				FileIcon: fileIcon(file.Name()),
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
				entries = append(entries, Entry{
					ID:       "link/" + url.PathEscape(line),
					Type:     "link",
					Content:  line,
					Filename: line,
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
		"style.css":    "text/css",
		"manifest.json": "application/json",
		"sw.js":        "application/javascript",
		"md.js":        "application/javascript",
		"favicon.ico":  "image/x-icon",
		"icon-192.png": "image/png",
		"icon-512.png": "image/png",
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
			if _, err := f.WriteString(submitContent + "\n"); err != nil {
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
						uniqueFileName := generateUniqueFilename("data/files", fileName)
						f, err := os.Create(filepath.Join("data/files", uniqueFileName))
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
				uniqueFileName := generateUniqueFilename("data/text", filename)
				if err := os.WriteFile(filepath.Join("data/text", uniqueFileName), []byte(submitContent), 0644); err != nil {
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

		// Update expiration tracker entry atomically
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

		if err := os.Rename(oldFullPath, newPath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
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
			linkToDelete := after
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
				if strings.TrimSpace(line) == strings.TrimSpace(linkToDelete) && !found {
					found = true // Remove only the first occurrence
					continue
				}
				if strings.TrimSpace(line) != "" {
					newLines = append(newLines, line)
				}
			}
			output := strings.Join(newLines, "\n")
			// Add newline for correctness
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
		oldURL, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/edit-link/"))
		if err != nil || oldURL == "" {
			http.Error(w, "Invalid URL", http.StatusBadRequest)
			return
		}
		newURL := r.FormValue("newurl")
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
			if strings.TrimSpace(line) == strings.TrimSpace(oldURL) && !found {
				newLines = append(newLines, newURL)
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
		log.Printf("Edited link %s -> %s\n", oldURL, newURL)
	})

	// Reorder links
	http.HandleFunc("/api/reorder-links", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var links []string
		if err := json.NewDecoder(r.Body).Decode(&links); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		// Validate each entry is a proper http/https URL
		for _, l := range links {
			u, err := url.ParseRequestURI(l)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "Invalid URL in list: "+l, http.StatusBadRequest)
				return
			}
		}
		output := strings.Join(links, "\n")
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

	// Start server
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

// isImageFile returns true for common web-displayable image extensions.
func isImageFile(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".svg", ".bmp", ".ico":
		return true
	}
	return false
}

// fileIcon returns a FontAwesome icon class for a given filename extension.
func fileIcon(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	// Images (fallback for non-previewable)
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".svg", ".bmp", ".ico":
		return "fa-file-image"
	// Documents
	case ".pdf":
		return "fa-file-pdf"
	case ".doc", ".docx":
		return "fa-file-word"
	case ".xls", ".xlsx":
		return "fa-file-excel"
	case ".ppt", ".pptx":
		return "fa-file-powerpoint"
	// Archives
	case ".zip", ".tar", ".gz", ".bz2", ".xz", ".7z", ".rar":
		return "fa-file-zipper"
	// Code / text
	case ".go", ".py", ".js", ".ts", ".rb", ".rs", ".c", ".cpp", ".h",
		".java", ".kt", ".swift", ".php", ".cs", ".sh", ".bash", ".ps1",
		".html", ".htm", ".css", ".scss", ".json", ".yaml", ".yml",
		".toml", ".xml", ".sql", ".md", ".dockerfile":
		return "fa-file-code"
	// Plain text
	case ".txt", ".log", ".csv", ".ini", ".conf", ".env":
		return "fa-file-lines"
	// Audio
	case ".mp3", ".wav", ".flac", ".ogg", ".aac", ".m4a":
		return "fa-file-audio"
	// Video
	case ".mp4", ".mkv", ".avi", ".mov", ".webm", ".flv":
		return "fa-file-video"
	// Font
	case ".ttf", ".otf", ".woff", ".woff2":
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
