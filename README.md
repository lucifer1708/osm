<div align="center">

```
 ██████╗ ███████╗███╗   ███╗
██╔═══██╗██╔════╝████╗ ████║
██║   ██║███████╗██╔████╔██║
██║   ██║╚════██║██║╚██╔╝██║
╚██████╔╝███████║██║ ╚═╝ ██║
 ╚═════╝ ╚══════╝╚═╝     ╚═╝
```

**Object Storage Manager**

*One UI to rule them all — S3, R2, MinIO, Hetzner, Backblaze and more.*

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://go.dev)
[![SQLite](https://img.shields.io/badge/SQLite-embedded-003B57?style=for-the-badge&logo=sqlite&logoColor=white)](https://sqlite.org)
[![HTMX](https://img.shields.io/badge/HTMX-powered-36C?style=for-the-badge&logo=htmx&logoColor=white)](https://htmx.org)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?style=for-the-badge&logo=docker&logoColor=white)](https://docker.com)
[![License](https://img.shields.io/badge/License-MIT-green?style=for-the-badge)](LICENSE)

</div>

---

## ✦ What is OSM?

**OSM** is a self-hosted, single-binary web UI for managing S3-compatible object storage. No Node.js, no heavy dependencies, no cloud accounts required. Drop in a binary (or a Docker image), point it at your storage endpoint, and get a full-featured file manager in your browser.

Built with **Go** on the backend, **HTMX** on the frontend, and **SQLite** for persistence — OSM is deliberately small, fast, and hackable.

---

## ⚡ Feature Highlights

<table>
<tr>
<td width="50%">

### 🗂 File Management
- Browse buckets, folders, and files
- Upload with drag-and-drop support
- Download, rename, move, delete
- Create folders and buckets
- Infinite scroll — handles millions of files without breaking a sweat
- Full-text search across all files in a folder (even with 100k+ objects)

</td>
<td width="50%">

### 🔒 Access Control
- Per-user bucket & folder permissions
- Read-only or write access per scope
- Wildcard bucket rules (`*`)
- Prefix-based inheritance (`photos/` covers `photos/vacation/`)
- UI hides write actions for read-only users

</td>
</tr>
<tr>
<td width="50%">

### 🌐 Public File Serving
- Toggle per-file public/private ACL
- Copy public link with one click
- Dedicated public files server on a separate port
- `PUBLIC_FILES_HOST` for custom CDN domains
- Cache-friendly headers (`Cache-Control: public`)

</td>
<td width="50%">

### 👤 Auth & Security
- Username + bcrypt password auth
- TOTP-based two-factor authentication (2FA)
- Trusted device tokens — 2FA remembered for 30 days per browser
- Multi-user with role-based admin
- Full audit log for every action
- Session management with auto-expiry

</td>
</tr>
<tr>
<td width="50%">

### 🛠 Admin Dashboard
- Create & delete user accounts
- Assign per-user bucket/folder permissions
- View audit log (login, upload, delete events)
- First registered user is automatically admin

</td>
<td width="50%">

### 🐳 Deployment
- Single statically-linked binary — no runtime deps
- Multi-arch Docker image (`linux/amd64` + `linux/arm64`)
- Docker Compose with persistent SQLite volume
- Dockerfile uses multi-stage build for minimal image size
- Health check endpoint built in

</td>
</tr>
</table>

---

## 🔌 Supported Providers

| Provider | ENDPOINT | REGION |
|---|---|---|
| **AWS S3** | *(leave blank)* | `us-east-1` |
| **Cloudflare R2** | `https://<account>.r2.cloudflarestorage.com` | `auto` |
| **Hetzner Object Storage** | `https://<location>.your-objectstorage.com` | `<location>` |
| **MinIO** | `http://localhost:9000` | `us-east-1` |
| **Backblaze B2** | `https://s3.<region>.backblazeb2.com` | `us-west-002` |
| **Wasabi** | `https://s3.wasabisys.com` | `us-east-1` |
| **Any S3-compatible** | your endpoint | your region |

---

## 🚀 Quick Start

### Option A — Docker Compose (recommended)

```bash
# 1. Grab the compose file
curl -O https://raw.githubusercontent.com/your-org/osm/main/docker-compose.yml

# 2. Create your .env
cp .env.example .env
# Edit .env with your storage credentials

# 3. Start
docker compose up -d
```

App is live at **http://localhost:8080**
Public files server at **http://localhost:9090**

---

### Option B — Build from source

```bash
# Clone
git clone https://github.com/your-org/osm
cd osm

# First-time setup (copies .env.example → .env, downloads deps)
make setup

# Edit your credentials
$EDITOR .env

# Run
make run
```

---

### Option C — Binary

```bash
# Build the binary
make build

# Set credentials inline or via .env
ACCESS_KEY=... SECRET_KEY=... ENDPOINT=... ./bin/osm
```

---

## ⚙️ Configuration

All config is via environment variables (or `.env` file):

```env
# ── Storage ────────────────────────────────────────────
ENDPOINT=                        # Leave blank for AWS S3
ACCESS_KEY=your-access-key
SECRET_KEY=your-secret-key
REGION=us-east-1

# ── App ────────────────────────────────────────────────
PORT=8080                        # Main UI port (default: 8080)
DB_PATH=./data/osm.db            # SQLite database path

# ── Public Files Server ────────────────────────────────
FILES_PORT=9090                  # Dedicated public files server port
PUBLIC_FILES_HOST=static.example.com  # CDN domain → generates pretty public URLs
                                 # Files served at: https://static.example.com/files/<bucket>/<key>
```

---

## 🌍 Public File Server

OSM runs a **second HTTP server** (no auth) specifically for serving public files:

```
http://your-host:9090/files/<bucket>/<key>
```

- Only serves files that are actually public on S3 — private files return `403`
- Set `PUBLIC_FILES_HOST=static.example.com` and point your domain/CDN at port `9090`
- All "copy public link" buttons in the UI will generate URLs using your custom domain
- Aggressive caching headers for CDN compatibility

```
# Nginx example: proxy static.example.com → OSM files server
server {
    server_name static.example.com;
    location / {
        proxy_pass http://localhost:9090;
    }
}
```

---

## 🔐 Two-Factor Authentication

OSM uses TOTP (compatible with Google Authenticator, Authy, 1Password, etc.):

1. Log in → you're redirected to **2FA setup** (scan QR code)
2. Verify the code → you're in
3. Next time you log in on the **same browser**, 2FA is skipped (trusted device for 30 days)
4. Log out → device trust is revoked
5. Reset 2FA → all trusted devices invalidated everywhere

---

## 👥 User & Permission Management

Admins can grant users access to specific buckets or folder paths:

| Rule | Effect |
|---|---|
| `bucket=*`, `prefix=` | Access to **all** buckets and paths |
| `bucket=photos`, `prefix=` | Access to the entire `photos` bucket |
| `bucket=photos`, `prefix=vacation/` | Access to `photos/vacation/` and everything inside |
| `access=read` | Can browse, download, preview — no write actions |
| `access=write` | Full read + upload, delete, rename, folder, ACL |

Rules are **hierarchical** — the most specific matching rule wins.

---

## 🏗 Architecture

```
┌─────────────────────────────────────────────────────────┐
│                        OSM Process                       │
│                                                         │
│  :8080  ┌─────────────┐    ┌──────────┐                │
│  ──────▶│ Auth Middle │───▶│ Handlers │                │
│         │    ware     │    │          │                │
│         └─────────────┘    └────┬─────┘                │
│                                 │                       │
│  :9090  ┌─────────────┐         │  ┌────────────────┐  │
│  ──────▶│ Public Files│    ┌────▼──▶│  AWS SDK v2    │  │
│         │  (no auth)  │    │    │   │  S3 Client     │  │
│         └─────────────┘    │    │   └────────────────┘  │
│                            │    │                       │
│                       ┌────▼──────────────┐            │
│                       │  SQLite (via WAL)  │            │
│                       │  users / sessions  │            │
│                       │  permissions / log │            │
│                       └────────────────────┘            │
└─────────────────────────────────────────────────────────┘
```

**Stack:**
- **Go 1.25** — standard library `net/http` with Go 1.22 routing (`{key...}` wildcards)
- **AWS SDK v2** — S3-compatible API calls, anonymous credential probing for ACL detection
- **SQLite** via `modernc.org/sqlite` — pure-Go, zero CGO, single file database
- **HTMX** — all dynamic UI interactions (infinite scroll, modals, partials) with zero custom JS framework
- **Tailwind CSS** — utility-first styling, dark mode support
- **bcrypt + TOTP** — `golang.org/x/crypto` + `pquerna/otp`

---

## 🐳 Docker

### Build and push multi-arch image

```bash
make docker-push
# Builds linux/amd64 + linux/arm64 and pushes to your registry
```

### Run with Docker Compose

```bash
make docker-up     # start (detached)
make docker-logs   # tail logs
make docker-down   # stop
```

### Dockerfile summary

```
Stage 1: golang:1.25-alpine
  → go build -ldflags="-s -w" CGO_ENABLED=0
  → statically linked binary

Stage 2: alpine:3.20
  → ca-certificates + tzdata only
  → non-root user (osm:osm)
  → EXPOSE 8080 9090
```

---

## 🛠 Makefile Reference

```
make setup         First-time setup (copy .env, tidy deps)
make create-user   Add a user to the database interactively
make run           Run from source   (PORT=8081 FILES_PORT=9090)
make dev           Live-reload with air
make build         Compile → ./bin/osm
make start         Build + run binary
make docker-build  Build Docker image (local platform)
make docker-push   Build amd64+arm64 and push to registry
make docker-up     Start via Docker Compose
make docker-down   Stop Docker Compose stack
make docker-logs   Tail container logs
make clean         Remove build artifacts
```

**Overrides:**
```bash
PORT=9090 make run
FILES_PORT=9091 make run
DB_PATH=/tmp/test.db make run
PUBLIC_FILES_HOST=static.example.com make run
```

---

## 📁 Project Structure

```
osm/
├── main.go                  # Server entry point, route registration
├── handlers/
│   ├── auth.go              # Login, logout, TOTP, session, admin user mgmt
│   └── storage.go           # S3 operations, ACL, public file server
├── db/
│   └── db.go                # SQLite schema, migrations, all DB helpers
├── cmd/
│   └── create-user/         # CLI tool to add a user directly to SQLite
├── templates/
│   ├── layout.html          # Base HTML layout + JS utilities
│   ├── index.html           # Main app shell
│   ├── auth/                # Login, setup, settings, TOTP pages
│   └── partials/            # HTMX-swapped fragments
│       ├── object-list.html # File browser panel (toolbar + table)
│       ├── object-rows.html # Table rows + infinite-scroll sentinel
│       ├── bucket-list.html # Sidebar bucket list
│       ├── acl-panel.html   # Public/private ACL toggle
│       └── user-perms.html  # Admin permission editor
├── static/                  # CSS, JS, favicon
├── Dockerfile
├── docker-compose.yml
└── Makefile
```

---

## 🤝 Contributing

PRs welcome. Keep it simple — OSM is intentionally dependency-light.

1. Fork → branch → change
2. `go build ./...` must pass
3. No new JS frameworks
4. Open PR

---

<div align="center">

Made with ☕ and a distaste for overengineered storage UIs.

**OSM** — *just works.*

</div>
