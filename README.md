<div align="center">
  <img src="assets/logo.svg" alt="Local Content Share Logo" width="200">
  <h1>Local Content Share</h1>

  <a href="https://github.com/gfunkmonk/local-content-share/actions/workflows/binary-build.yml"><img alt="Build Workflow" src="https://github.com/gfunkmonk/local-content-share/actions/workflows/binary-build.yml/badge.svg"></a>&nbsp;<a href="https://github.com/gfunkmonk/local-content-share/actions/workflows/docker-publish.yml"><img alt="Container Workflow" src="https://github.com/gfunkmonk/local-content-share/actions/workflows/docker-publish.yml/badge.svg"></a><br>
  <a href="https://github.com/gfunkmonk/local-content-share/releases"><img alt="GitHub Release" src="https://img.shields.io/github/v/release/gfunkmonk/local-content-share"></a>&nbsp;<a href="https://hub.docker.com/r/gfunkmonk/local-content-share"><img alt="Docker Pulls" src="https://img.shields.io/docker/pulls/gfunkmonk/local-content-share"></a><br><br>
  <a href="#screenshots">Screenshots</a> &bull; <a href="#installation-and-usage">Install & Use</a> &bull; <a href="#tips-and-notes">Tips & Notes</a>
</div>

---

A simple & elegant self-hosted app for **storing/sharing text snippets, files, and links** in your **local network** with **no setup on client devices**. Think of this as an *all-in-one alternative* to **airdrop**, **local-pastebin**, and a **scratchpad**.

## Features

### Snippets
- Create, edit, rename, and delete plain text snippets
- **Syntax highlighting** in the viewer for 35+ languages — language is detected from the filename extension
- **Copy content** or **copy raw URL** (`/raw/text/filename`) to share a direct link to the plain text
- View snippets in a modal with a close button or Escape key

### Files
- Upload files via click, drag-and-drop, or clipboard paste
- **Image thumbnails** shown inline on file cards
- **Video thumbnails** generated from the first frame of the video
- **File type icons** for non-image/video files (PDF, archive, code, audio, etc.)
- Viewable files (images, video, audio, PDF, text, code) open in-browser by clicking the thumbnail or icon
- **Download** files with a single click; **copy the download URL** to share with others on the LAN
- When uploading with a custom name, the original file's extension is automatically appended
- Rename files in-place

### Links
- Store and share URLs with an optional **display name** separate from the URL
- **Globe icon** and truncation with tooltip on long URLs
- **Drag to reorder** links — new order persists across page loads
- Edit or delete individual links

### Notepad
- Persistent markdown scratchpad shared across all devices
- Toggle between edit and reader (rendered preview) modes
- Undo/redo history; content auto-saves after a period of inactivity

### Organisation
- **Sort** snippets and files by name, size, or date — sort preference saved in `localStorage`
- **Empty state messages** when a section has no content
- Expiration (TTL) per file or snippet: Never, 1 hour, 4 hours, 1 day, or Custom format (`34m`, `3w`, `2M`, `11d`)
- Set a default expiry with the `DEFAULT_EXPIRY` environment variable

### Settings
- **Dark / Light / System** theme toggle — dark is the default; preference saved in `localStorage`
- **Export** all data as a `.zip` archive (files, snippets, links, notepad, expirations)
- **Import** a previously exported `.zip` to restore or merge data
- **Bulk delete** — enable bulk delete mode from Settings, check items across all sections, delete in one action

### Live Updates & UX
- **SSE** (Server-Sent Events) keeps all open clients in sync — new/edited/deleted content appears automatically
- **Reconnects** when a tab becomes visible again after being hidden
- Sort preferences, theme, and layout survive page reloads via `localStorage`
- Card **entrance animations** with a subtle stagger
- Escape key closes any open modal
- Confirm before closing the new item form if unsaved content is present
- **PWA** (Progressive Web App) support — installable on mobile and desktop

### Security & Reliability
- Path traversal protection on all file-serving endpoints
- File names are sanitized — only filesystem-illegal characters (`/ \ : * ? " < > |`) are replaced
- Expiration tracker uses a read/write mutex to avoid deadlocks under concurrent access
- MIME type detection via the standard library with content sniffing fallback

---

> [!NOTE]
> This application is meant to be deployed within your homelab only. There is no authentication mechanism implemented. If you are exposing to the public, ensure there is authentication fronting it and non-destructive users using it.

## Screenshots

| | Desktop View | Mobile View |
| --- | --- | --- |
| Light | <img src="assets/dlight.png" alt="Light"> | <img src="assets/mlight.png" alt="Light"> |
| Dark | <img src="assets/ddark.png" alt="Dark"> | <img src="assets/mdark.png" alt="Dark"> |

## Installation and Usage

### Using Docker (Recommended for Self-Hosting)

Use `docker` CLI one liner and setup a persistence directory (so a container failure does not delete your data):

```bash
mkdir $HOME/.localcontentshare
```
```bash
docker run --name local-content-share \
  -p 8080:8080 \
  -v $HOME/.localcontentshare:/app/data \
  gfunkmonk/local-content-share:main
```

The application will be available at `http://localhost:8080` (or your server IP).

You can also use the following compose file with container managers like Portainer and Dockge (remember to change the mounted volume):

```yaml
services:
  contentshare:
    image: gfunkmonk/local-content-share:main
    container_name: local-content-share
    volumes:
      - /home/user/lcshare:/app/data # Change as needed
    ports:
      - 8080:8080
```

### Using Binary

Download the appropriate binary for your system from the [latest release](https://github.com/gfunkmonk/local-content-share/releases/latest).

Binaries are provided for:

| OS | Architectures |
|---|---|
| Linux | amd64, arm64, armv6, armv7, 386, riscv64, and more |
| macOS | amd64, arm64 |
| Windows | amd64, arm64, armv6, armv7, 386 |
| FreeBSD / NetBSD / OpenBSD | amd64, arm64, arm, 386 |

Make the binary executable (Linux/macOS) with `chmod +x local-content-share-*` and run it. The app will be available at `http://localhost:8080`.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--listen` | `:8080` | `host:port` the server listens on |

### Local Development

With `Go 1.23+` installed:

```bash
git clone https://github.com/gfunkmonk/local-content-share.git
cd local-content-share
go build .
./local-content-share
```

Or install directly to your `GOBIN`:

```bash
go install github.com/gfunkmonk/local-content-share@latest
```

## Tips and Notes

### Snippets
- Type or paste text into the content area and click **Submit** to save a snippet
- Optionally provide a name — otherwise it defaults to a timestamp
- Click the **pen icon** to edit both the content and the name of a snippet in one step
- Click a snippet card to open a viewer with syntax highlighting
- Click the **copy icon** to copy the content, or the **link icon** to copy the raw URL

### Files
- Click the upload area, drag files onto it, or paste an image from your clipboard
- When providing a custom name, the original extension is automatically appended on blur or submit
- Click the **thumbnail or icon** on the left of a card to view the file in a new tab (images, video, PDF, text, etc.)
- Click the **download icon** to save the file, or the **link icon** to copy the download URL

### Links
- Click **Link** to add a URL with an optional display name
- Hover over a truncated URL to see the full address in a tooltip
- Drag the grip handle on the left to reorder links — the order persists on the server
- Click the **pen icon** on a link card to edit both its name and URL

### Bulk Delete
1. Open **Settings** (gear icon, top-right)
2. Click **Select items**
3. Check the items you want to delete across any section
4. Click **Delete selected** in the floating bar at the bottom
5. Click **Cancel** to exit select mode without deleting

### Export & Import
- **Export**: Settings → *Export all data* — downloads a `.zip` containing all files, snippets, links, the notepad, and expiration data
- **Import**: Settings → *Import data* — select a previously exported `.zip`; contents are merged into the current data directory (existing files with the same name are overwritten)

### Expiration (TTL)
- Click the **clock / expiry button** in the new item form to cycle through: Never → 1 hour → 4 hours → 1 day → Custom
- Custom format: `NT` where N is a number and T is `m` (minute), `h` (hour), `d` (day), `w` (week), `M` (month), or `y` (year) — e.g. `30m`, `2d`, `1w`
- Set `DEFAULT_EXPIRY` environment variable to change the default (e.g. `DEFAULT_EXPIRY=1d`)

### Theme
- Open **Settings** and choose Dark, Light, or System
- Dark is the default for new users; the preference is saved in `localStorage`

### Notepad
- Persistent markdown editor shared across all connected devices
- Toggle between **write** and **reader** (rendered markdown) mode with the book icon
- Content auto-saves after ~2 seconds of inactivity; also saved on page close

### A Note on Reverse Proxies

Reverse proxies are fairly common in homelab settings. Two features can be affected:

- **File size**: reverse proxy software may impose upload size limits; Local Content Share does not
- **Upload progress**: buffering in some proxy configs delays progress bar updates until upload completes

Sample fix for Nginx Proxy Manager — in the proxy host's Advanced tab:

```nginx
client_max_body_size 5G;
proxy_request_buffering off;
proxy_buffering off;
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;
proxy_connect_timeout 3600s;
```

### Backend Data Structure

The `data` directory holds all content:

```
data/
  files/          uploaded files
  text/           text snippets
  notepad/
    md.file       notepad content
  links.file      stored links (name|url or bare url, one per line)
  expirations.json  expiration timestamps
```

The app needs write permissions for the directory it runs in.
