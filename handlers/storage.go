package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/lucifer1708/object_storage_manager/db"
)

// ─── Permission helpers ───────────────────────────────────────────────────────

// canAccess checks whether the current user has the required access level on
// the given bucket+path. Admins always pass. Non-admins use the permission table.
// op must be "read" or "write".
func canAccess(r *http.Request, bucket, path, op string) bool {
	sess := CurrentSession(r)
	if sess == nil {
		return false
	}
	user, _ := db.GetUserByID(sess.UserID)
	if user == nil {
		return false
	}
	if user.IsAdmin {
		return true
	}
	effective := db.EffectiveAccess(sess.UserID, bucket, path)
	switch op {
	case "read":
		return effective == "read" || effective == "write"
	case "write":
		return effective == "write"
	}
	return false
}

// denyAccess sends an appropriate "access denied" response for storage handlers.
func denyAccess(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`<div class="m-4 p-4 bg-red-50 dark:bg-red-900/20 border border-red-200 dark:border-red-800 rounded-xl text-red-600 dark:text-red-400 text-sm flex items-center gap-2"><svg class="w-4 h-4 flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z"/></svg>Access denied — you don't have permission for this action.</div>`))
		return
	}
	http.Error(w, "Access denied", http.StatusForbidden)
}

// Session holds the S3 client and config
type Session struct {
	Client   *s3.Client
	Endpoint string
	Region   string
	mu       sync.RWMutex
}

var session = &Session{}

func getClient() *s3.Client {
	session.mu.RLock()
	defer session.mu.RUnlock()
	return session.Client
}

func isConnected() bool {
	return getClient() != nil
}

// AutoConnect tries to connect using environment variables at startup.
func AutoConnect() {
	accessKey := os.Getenv("ACCESS_KEY")
	secretKey := os.Getenv("SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		return
	}
	endpoint := os.Getenv("ENDPOINT")
	region := os.Getenv("REGION")
	if region == "" {
		region = "us-east-1"
	}

	client, err := buildClient(endpoint, accessKey, secretKey, region)
	if err != nil {
		log.Printf("auto-connect: failed to build client: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		log.Printf("auto-connect: connection test failed: %v", err)
		return
	}

	session.mu.Lock()
	session.Client = client
	session.Endpoint = endpoint
	session.Region = region
	session.mu.Unlock()
	log.Printf("auto-connect: connected (endpoint=%q region=%s)", endpoint, region)
}

// normalizeEndpoint ensures the endpoint URL has a scheme.
func normalizeEndpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return "https://" + endpoint
	}
	return endpoint
}

func buildClient(endpoint, accessKey, secretKey, region string) (*s3.Client, error) {
	endpoint = normalizeEndpoint(endpoint)

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, err
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	}), nil
}

// templates cache
var tmplCache = map[string]*template.Template{}
var tmplMu sync.RWMutex

func getTemplate(name string, files ...string) *template.Template {
	key := strings.Join(files, "|")
	tmplMu.RLock()
	t, ok := tmplCache[key]
	tmplMu.RUnlock()
	if ok {
		return t
	}
	t = template.Must(template.New(name).Funcs(funcMap()).ParseFiles(files...))
	tmplMu.Lock()
	tmplCache[key] = t
	tmplMu.Unlock()
	return t
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		"formatSize": func(size int64) string {
			const unit = 1024
			if size < unit {
				return fmt.Sprintf("%d B", size)
			}
			div, exp := int64(unit), 0
			for n := size / unit; n >= unit; n /= unit {
				div *= unit
				exp++
			}
			return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
		},
		"formatTime": func(t *time.Time) string {
			if t == nil {
				return "-"
			}
			return t.Format("Jan 02, 2006 15:04")
		},
		"isImage": func(key string) bool {
			ext := strings.ToLower(filepath.Ext(key))
			return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp" || ext == ".svg"
		},
		"isVideo": func(key string) bool {
			ext := strings.ToLower(filepath.Ext(key))
			return ext == ".mp4" || ext == ".webm" || ext == ".ogg"
		},
		"isPDF": func(key string) bool {
			return strings.ToLower(filepath.Ext(key)) == ".pdf"
		},
		"isText": func(key string) bool {
			ext := strings.ToLower(filepath.Ext(key))
			textExts := []string{".txt", ".md", ".json", ".xml", ".yaml", ".yml", ".csv", ".log", ".sh", ".py", ".js", ".ts", ".go", ".html", ".css", ".sql"}
			for _, e := range textExts {
				if ext == e {
					return true
				}
			}
			return false
		},
		"fileIcon": fileIcon,
		"basename": func(key string) string {
			parts := strings.Split(strings.TrimSuffix(key, "/"), "/")
			return parts[len(parts)-1]
		},
		"isFolder": func(key string) bool {
			return strings.HasSuffix(key, "/")
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"urlEncode":  url.QueryEscape,
		"join": strings.Join,
		"trimPrefix": strings.TrimPrefix,
		"trimSuffix": strings.TrimSuffix,
		"hasPrefix":  strings.HasPrefix,
		"hasSuffix":  strings.HasSuffix,
		"contains":   strings.Contains,
		"split":      strings.Split,
		"last": func(s []string) string {
			if len(s) == 0 {
				return ""
			}
			return s[len(s)-1]
		},
		"pathBreadcrumbs": func(prefix string) []map[string]string {
			if prefix == "" {
				return nil
			}
			parts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
			var crumbs []map[string]string
			var cumulative string
			for _, p := range parts {
				if p == "" {
					continue
				}
				cumulative += p + "/"
				crumbs = append(crumbs, map[string]string{
					"name": p,
					"path": cumulative,
				})
			}
			return crumbs
		},
	}
}

func fileIcon(key string) string {
	if strings.HasSuffix(key, "/") {
		return "📁"
	}
	ext := strings.ToLower(filepath.Ext(key))
	icons := map[string]string{
		".jpg": "🖼️", ".jpeg": "🖼️", ".png": "🖼️", ".gif": "🖼️", ".webp": "🖼️", ".svg": "🖼️",
		".mp4": "🎬", ".webm": "🎬", ".ogg": "🎬", ".avi": "🎬", ".mov": "🎬",
		".mp3": "🎵", ".wav": "🎵", ".flac": "🎵", ".aac": "🎵",
		".pdf": "📄",
		".doc": "📝", ".docx": "📝",
		".xls": "📊", ".xlsx": "📊", ".csv": "📊",
		".zip": "🗜️", ".tar": "🗜️", ".gz": "🗜️", ".rar": "🗜️",
		".go": "💻", ".js": "💻", ".ts": "💻", ".py": "💻", ".sh": "💻",
		".json": "📋", ".yaml": "📋", ".yml": "📋", ".xml": "📋",
		".html": "🌐", ".css": "🎨",
		".txt": "📄", ".md": "📄", ".log": "📄",
		".sql": "🗃️",
	}
	if icon, ok := icons[ext]; ok {
		return icon
	}
	return "📄"
}

// ObjectInfo represents a file or folder in storage
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified *time.Time
	IsFolder     bool
	ContentType  string
	ETag         string
}

// ---- Handlers ----

// envHasStorageConfig returns true when storage credentials are provided via env/config,
// meaning the user should never see the manual connect form.
func envHasStorageConfig() bool {
	return os.Getenv("ACCESS_KEY") != "" && os.Getenv("SECRET_KEY") != ""
}

func Index(w http.ResponseWriter, r *http.Request) {
	managed := envHasStorageConfig()

	// If managed mode but not connected, try again (e.g. transient startup failure)
	if managed && !isConnected() {
		AutoConnect()
	}

	data := map[string]interface{}{
		"Connected":       isConnected(),
		"Managed":         managed,
		"DefaultEndpoint": os.Getenv("ENDPOINT"),
		"DefaultRegion": func() string {
			if r := os.Getenv("REGION"); r != "" {
				return r
			}
			return "us-east-1"
		}(),
	}

	if isConnected() {
		buckets, err := listBuckets()
		if err == nil {
			data["Buckets"] = buckets
		} else {
			data["ConnectError"] = err.Error()
		}
		data["CanManageBuckets"] = canAccess(r, "*", "", "write")
	}

	renderTemplate(w, "index", data)
}

func Connect(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	endpoint := r.FormValue("endpoint")
	accessKey := r.FormValue("access_key")
	secretKey := r.FormValue("secret_key")
	region := r.FormValue("region")
	if region == "" {
		region = "us-east-1"
	}

	client, err := buildClient(endpoint, accessKey, secretKey, region)
	if err != nil {
		renderError(w, "Failed to create config: "+err.Error())
		return
	}

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		renderError(w, "Connection failed: "+err.Error())
		return
	}

	session.mu.Lock()
	session.Client = client
	session.Endpoint = endpoint
	session.Region = region
	session.mu.Unlock()

	// Invalidate template cache on reconnect
	tmplMu.Lock()
	tmplCache = map[string]*template.Template{}
	tmplMu.Unlock()

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func Disconnect(w http.ResponseWriter, r *http.Request) {
	session.mu.Lock()
	session.Client = nil
	session.mu.Unlock()

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func ListBuckets(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}

	buckets, err := listBuckets()
	if err != nil {
		renderError(w, err.Error())
		return
	}

	renderPartial(w, "bucket-list", map[string]interface{}{
		"Buckets":          buckets,
		"CanManageBuckets": canAccess(r, "*", "", "write"),
	})
}

func CreateBucket(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}
	if !canAccess(r, "*", "", "write") {
		denyAccess(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		renderError(w, err.Error())
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		renderError(w, "Bucket name is required")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	input := &s3.CreateBucketInput{Bucket: aws.String(name)}
	if session.Region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(session.Region),
		}
	}

	_, err := getClient().CreateBucket(ctx, input)
	if err != nil {
		renderError(w, "Failed to create bucket: "+err.Error())
		return
	}

	// Return updated bucket list
	buckets, _ := listBuckets()
	renderPartial(w, "bucket-list", map[string]interface{}{
		"Buckets":          buckets,
		"CanManageBuckets": true, // already verified above
		"Toast":            "Bucket '" + name + "' created successfully",
		"ToastOK":          true,
	})
}

func DeleteBucket(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}
	if !canAccess(r, "*", "", "write") {
		denyAccess(w, r)
		return
	}

	bucket := r.PathValue("bucket")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := getClient().DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		renderError(w, "Failed to delete bucket: "+err.Error())
		return
	}

	buckets, _ := listBuckets()
	renderPartial(w, "bucket-list", map[string]interface{}{
		"Buckets":          buckets,
		"CanManageBuckets": true, // already verified above
		"Toast":            "Bucket '" + bucket + "' deleted",
		"ToastOK":          true,
	})
}

func ListObjects(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}

	bucket := r.PathValue("bucket")
	prefix := r.URL.Query().Get("prefix")
	search := r.URL.Query().Get("search")
	sortBy := r.URL.Query().Get("sort")
	sortDir := r.URL.Query().Get("dir")
	token := r.URL.Query().Get("token") // S3 continuation token for infinite scroll

	if !canAccess(r, bucket, prefix, "read") {
		denyAccess(w, r)
		return
	}

	// Infinite scroll "load more" — return just the next batch of rows.
	// Only used in browse mode (no search, no non-default sort).
	if token != "" && search == "" {
		objects, nextToken, err := listObjectsPage(bucket, prefix, token)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		renderPartial(w, "object-rows", map[string]interface{}{
			"Bucket":    bucket,
			"Prefix":    prefix,
			"Objects":   objects,
			"NextToken": nextToken,
			"SortBy":    sortBy,
			"SortDir":   sortDir,
			"CanWrite":  canAccess(r, bucket, prefix, "write"),
		})
		return
	}

	var objects []ObjectInfo
	var nextToken string

	// Search or non-default sort requires loading all objects for correctness.
	defaultSort := (sortBy == "" || sortBy == "name") && (sortDir == "" || sortDir == "asc")
	if search != "" || !defaultSort {
		var err error
		objects, err = listObjects(bucket, prefix)
		if err != nil {
			renderError(w, err.Error())
			return
		}
		if search != "" {
			var filtered []ObjectInfo
			for _, o := range objects {
				name := filepath.Base(strings.TrimSuffix(o.Key, "/"))
				if strings.Contains(strings.ToLower(name), strings.ToLower(search)) {
					filtered = append(filtered, o)
				}
			}
			objects = filtered
		}
		sortObjects(objects, sortBy, sortDir)
	} else {
		// Browse mode: load first page only.
		var err error
		objects, nextToken, err = listObjectsPage(bucket, prefix, "")
		if err != nil {
			renderError(w, err.Error())
			return
		}
	}

	totalSize, fileCount := objectStats(objects)
	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":    bucket,
		"Prefix":    prefix,
		"Objects":   objects,
		"Search":    search,
		"SortBy":    sortBy,
		"SortDir":   sortDir,
		"TotalSize": totalSize,
		"FileCount": fileCount,
		"NextToken": nextToken,
		"HasMore":   nextToken != "",
		"CanWrite":  canAccess(r, bucket, prefix, "write"),
	})
}

func UploadObject(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}

	bucket := r.PathValue("bucket")

	// 500MB max upload
	if err := r.ParseMultipartForm(500 << 20); err != nil {
		renderError(w, "Failed to parse form: "+err.Error())
		return
	}

	prefix := r.FormValue("prefix")

	if !canAccess(r, bucket, prefix, "write") {
		denyAccess(w, r)
		return
	}
	makePublic := r.FormValue("acl") == "public-read"
	files := r.MultipartForm.File["files"]

	if len(files) == 0 {
		renderError(w, "No files selected")
		return
	}

	var uploaded []string
	var errs []string

	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			errs = append(errs, fh.Filename+": "+err.Error())
			continue
		}
		defer f.Close()

		key := prefix + fh.Filename
		contentType := fh.Header.Get("Content-Type")
		if contentType == "" {
			contentType = mime.TypeByExtension(filepath.Ext(fh.Filename))
		}
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		input := &s3.PutObjectInput{
			Bucket:      aws.String(bucket),
			Key:         aws.String(key),
			Body:        f,
			ContentType: aws.String(contentType),
		}
		if makePublic {
			input.ACL = types.ObjectCannedACLPublicRead
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		_, err = getClient().PutObject(ctx, input)
		cancel()
		if err != nil {
			errs = append(errs, fh.Filename+": "+err.Error())
			continue
		}
		aclStateCache.Store(aclCacheKey(bucket, key), makePublic)
		uploaded = append(uploaded, fh.Filename)
	}

	msg := fmt.Sprintf("Uploaded %d file(s)", len(uploaded))
	if len(errs) > 0 {
		msg += fmt.Sprintf(", %d failed: %s", len(errs), strings.Join(errs, "; "))
	}

	objects, _ := listObjects(bucket, prefix)
	sortObjects(objects, "", "")
	totalSize, fileCount := objectStats(objects)

	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":    bucket,
		"Prefix":    prefix,
		"Objects":   objects,
		"TotalSize": totalSize,
		"FileCount": fileCount,
		"Toast":     msg,
		"ToastOK":   len(errs) == 0,
		"CanWrite":  canAccess(r, bucket, prefix, "write"),
	})
}

func DownloadObject(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		http.Error(w, "Not connected", http.StatusUnauthorized)
		return
	}

	bucket := r.PathValue("bucket")
	key := r.PathValue("key")

	if !canAccess(r, bucket, key, "read") {
		http.Error(w, "Access denied", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := getClient().GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		http.Error(w, "Failed to get object: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer result.Body.Close()

	filename := filepath.Base(key)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	if result.ContentType != nil {
		w.Header().Set("Content-Type", *result.ContentType)
	}
	if result.ContentLength != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*result.ContentLength, 10))
	}

	io.Copy(w, result.Body)
}

func DeleteObject(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}

	bucket := r.PathValue("bucket")
	key := r.PathValue("key")
	prefix := r.URL.Query().Get("prefix")

	if !canAccess(r, bucket, key, "write") {
		denyAccess(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// If it's a folder, delete all contents
	if strings.HasSuffix(key, "/") {
		err := deleteFolder(ctx, bucket, key)
		if err != nil {
			renderError(w, err.Error())
			return
		}
	} else {
		_, err := getClient().DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			renderError(w, "Failed to delete: "+err.Error())
			return
		}
		aclStateCache.Delete(aclCacheKey(bucket, key))
	}

	objects, _ := listObjects(bucket, prefix)
	sortObjects(objects, "", "")

	totalSize, fileCount := objectStats(objects)
	name := filepath.Base(strings.TrimSuffix(key, "/"))
	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":    bucket,
		"Prefix":    prefix,
		"Objects":   objects,
		"TotalSize": totalSize,
		"FileCount": fileCount,
		"Toast":     "'" + name + "' deleted successfully",
		"ToastOK":   true,
		"CanWrite":  canAccess(r, bucket, prefix, "write"),
	})
}

func BulkDeleteObjects(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}
	bucket := r.PathValue("bucket")
	if !canAccess(r, bucket, "", "write") {
		denyAccess(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		renderError(w, err.Error())
		return
	}
	keys := r.Form["keys[]"]
	prefix := r.FormValue("prefix")
	if len(keys) == 0 {
		renderError(w, "No keys provided")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var deleted int
	var errs []string
	for _, key := range keys {
		if strings.HasSuffix(key, "/") {
			if err := deleteFolder(ctx, bucket, key); err != nil {
				errs = append(errs, err.Error())
				continue
			}
		} else {
			_, err := getClient().DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
			})
			if err != nil {
				errs = append(errs, "Failed to delete "+key+": "+err.Error())
				continue
			}
			aclStateCache.Delete(aclCacheKey(bucket, key))
		}
		deleted++
	}

	objects, _ := listObjects(bucket, prefix)
	sortObjects(objects, "", "")
	totalSize, fileCount := objectStats(objects)

	toast := fmt.Sprintf("%d item(s) deleted", deleted)
	toastOK := true
	if len(errs) > 0 {
		toast = fmt.Sprintf("%d deleted, %d failed", deleted, len(errs))
		toastOK = false
	}

	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":    bucket,
		"Prefix":    prefix,
		"Objects":   objects,
		"TotalSize": totalSize,
		"FileCount": fileCount,
		"Toast":     toast,
		"ToastOK":   toastOK,
		"CanWrite":  canAccess(r, bucket, prefix, "write"),
	})
}

func BulkSetACL(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}
	bucket := r.PathValue("bucket")
	if !canAccess(r, bucket, "", "write") {
		denyAccess(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		renderError(w, err.Error())
		return
	}
	keys := r.Form["keys[]"]
	aclVal := r.FormValue("acl")
	prefix := r.FormValue("prefix")
	if len(keys) == 0 {
		renderError(w, "No keys provided")
		return
	}
	if aclVal != "public-read" && aclVal != "private" {
		renderError(w, "Invalid ACL value")
		return
	}

	var cannedACL types.ObjectCannedACL
	if aclVal == "public-read" {
		cannedACL = types.ObjectCannedACLPublicRead
	} else {
		cannedACL = types.ObjectCannedACLPrivate
	}
	isPublic := aclVal == "public-read"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Expand folder keys to individual object keys
	var resolved []string
	for _, key := range keys {
		if strings.HasSuffix(key, "/") {
			objs, err := listObjects(bucket, key)
			if err != nil {
				renderError(w, "Failed to list folder "+key+": "+err.Error())
				return
			}
			for _, obj := range objs {
				if !obj.IsFolder {
					resolved = append(resolved, obj.Key)
				}
			}
		} else {
			resolved = append(resolved, key)
		}
	}
	if len(resolved) == 0 {
		renderError(w, "No objects found to update ACL")
		return
	}

	setACLOnKey := func(key string) error {
		_, err := getClient().PutObjectAcl(ctx, &s3.PutObjectAclInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			ACL:    cannedACL,
		})
		if err != nil {
			_, err = getClient().CopyObject(ctx, &s3.CopyObjectInput{
				Bucket:            aws.String(bucket),
				CopySource:        aws.String(bucket + "/" + key),
				Key:               aws.String(key),
				ACL:               cannedACL,
				MetadataDirective: types.MetadataDirectiveCopy,
			})
		}
		return err
	}

	var updated int
	var errs []string
	for _, key := range resolved {
		if err := setACLOnKey(key); err != nil {
			errs = append(errs, "Failed "+key+": "+err.Error())
			continue
		}
		aclStateCache.Store(aclCacheKey(bucket, key), isPublic)
		updated++
	}

	objects, _ := listObjects(bucket, prefix)
	sortObjects(objects, "", "")
	totalSize, fileCount := objectStats(objects)

	toast := fmt.Sprintf("ACL updated for %d item(s)", updated)
	toastOK := true
	if len(errs) > 0 {
		toast = fmt.Sprintf("%d updated, %d failed", updated, len(errs))
		toastOK = false
	}

	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":    bucket,
		"Prefix":    prefix,
		"Objects":   objects,
		"TotalSize": totalSize,
		"FileCount": fileCount,
		"Toast":     toast,
		"ToastOK":   toastOK,
		"CanWrite":  canAccess(r, bucket, prefix, "write"),
	})
}

func PresignObject(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}

	bucket := r.PathValue("bucket")
	key := r.PathValue("key")

	if !canAccess(r, bucket, key, "read") {
		denyAccess(w, r)
		return
	}
	expStr := r.URL.Query().Get("exp")
	expHours := 24
	if expStr != "" {
		if n, err := strconv.Atoi(expStr); err == nil && n > 0 {
			expHours = n
		}
	}

	presigner := s3.NewPresignClient(getClient())
	req, err := presigner.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(time.Duration(expHours)*time.Hour))
	if err != nil {
		renderError(w, "Failed to presign: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": req.URL})
}

func CreateFolder(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}

	bucket := r.PathValue("bucket")
	if err := r.ParseForm(); err != nil {
		renderError(w, err.Error())
		return
	}

	prefix := r.FormValue("prefix")
	folderName := strings.TrimSpace(r.FormValue("name"))
	if folderName == "" {
		renderError(w, "Folder name is required")
		return
	}

	if !canAccess(r, bucket, prefix, "write") {
		denyAccess(w, r)
		return
	}
	folderName = strings.Trim(folderName, "/")
	key := prefix + folderName + "/"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := getClient().PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          strings.NewReader(""),
		ContentLength: aws.Int64(0),
		ContentType:   aws.String("application/x-directory"),
	})
	if err != nil {
		renderError(w, "Failed to create folder: "+err.Error())
		return
	}

	objects, _ := listObjects(bucket, prefix)
	sortObjects(objects, "", "")

	totalSize, fileCount := objectStats(objects)
	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":    bucket,
		"Prefix":    prefix,
		"Objects":   objects,
		"TotalSize": totalSize,
		"FileCount": fileCount,
		"Toast":     "Folder '" + folderName + "' created",
		"ToastOK":   true,
		"CanWrite":  canAccess(r, bucket, prefix, "write"),
	})
}

func PreviewObject(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		http.Error(w, "Not connected", http.StatusUnauthorized)
		return
	}

	bucket := r.PathValue("bucket")
	key := r.PathValue("key")

	if !canAccess(r, bucket, key, "read") {
		http.Error(w, "Access denied", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := getClient().GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		http.Error(w, "Failed to get object: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer result.Body.Close()

	if result.ContentType != nil {
		w.Header().Set("Content-Type", *result.ContentType)
	}
	if result.ContentLength != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*result.ContentLength, 10))
	}

	io.Copy(w, result.Body)
}

func CopyObject(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}

	bucket := r.PathValue("bucket")
	if err := r.ParseForm(); err != nil {
		renderError(w, err.Error())
		return
	}

	sourceKey := r.FormValue("source")
	destKey := r.FormValue("dest")
	prefix := r.FormValue("prefix")

	if !canAccess(r, bucket, sourceKey, "write") {
		denyAccess(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := getClient().CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(bucket),
		CopySource: aws.String(bucket + "/" + sourceKey),
		Key:        aws.String(destKey),
	})
	if err != nil {
		renderError(w, "Failed to copy/rename: "+err.Error())
		return
	}

	// If rename, delete original and move cache entry
	if r.FormValue("move") == "true" {
		_, _ = getClient().DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(sourceKey),
		})
		if v, ok := aclStateCache.Load(aclCacheKey(bucket, sourceKey)); ok {
			aclStateCache.Store(aclCacheKey(bucket, destKey), v)
			aclStateCache.Delete(aclCacheKey(bucket, sourceKey))
		}
	}

	objects, _ := listObjects(bucket, prefix)
	sortObjects(objects, "", "")

	totalSize, fileCount := objectStats(objects)
	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":    bucket,
		"Prefix":    prefix,
		"Objects":   objects,
		"TotalSize": totalSize,
		"FileCount": fileCount,
		"Toast":     "Operation completed successfully",
		"ToastOK":   true,
		"CanWrite":  canAccess(r, bucket, prefix, "write"),
	})
}

// checkPublicAccessS3 tests whether an object is publicly readable by issuing
// a HeadObject with anonymous (unsigned) credentials against the same endpoint
// that the authenticated client uses. This is provider-agnostic and avoids
// fragile virtual-hosted-style URL guessing.
func checkPublicAccessS3(bucket, key string) bool {
	session.mu.RLock()
	endpoint := session.Endpoint
	region := session.Region
	session.mu.RUnlock()

	endpoint = normalizeEndpoint(endpoint)
	if region == "" {
		region = "us-east-1"
	}

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(aws.AnonymousCredentials{}),
	)
	if err != nil {
		return false
	}

	anonClient := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = anonClient.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	return err == nil
}

// GetACL checks the ACL for a single object and returns an HTML fragment.
func GetACL(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}
	bucket := r.PathValue("bucket")
	key := r.PathValue("key")

	if !canAccess(r, bucket, key, "read") {
		denyAccess(w, r)
		return
	}

	isPublic := resolvePublicState(bucket, key)
	pubURL := ""
	if isPublic {
		pubURL = publicURL(bucket, key)
	}

	renderPartial(w, "acl-panel", map[string]interface{}{
		"Bucket":   bucket,
		"Key":      key,
		"IsPublic": isPublic,
		"PublicURL": pubURL,
		"ACLError": "",
		"CanWrite": canAccess(r, bucket, key, "write"),
	})
}

// GetACLJSON returns JSON with public status and URL — used by the copy-link button.
func GetACLJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !isConnected() {
		json.NewEncoder(w).Encode(map[string]interface{}{"public": false, "url": ""})
		return
	}
	bucket := r.PathValue("bucket")
	key := r.PathValue("key")

	isPublic := resolvePublicState(bucket, key)
	pubURL := ""
	if isPublic {
		pubURL = publicURL(bucket, key)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"public": isPublic, "url": pubURL})
}

// SetACL sets public-read or private ACL on an object. Requires write access.
func SetACL(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}
	bucket := r.PathValue("bucket")
	key := r.PathValue("key")
	if !canAccess(r, bucket, key, "write") {
		denyAccess(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		renderError(w, err.Error())
		return
	}
	acl := r.FormValue("acl") // "public-read" or "private"

	var cannedACL types.ObjectCannedACL
	if acl == "public-read" {
		cannedACL = types.ObjectCannedACLPublicRead
	} else {
		cannedACL = types.ObjectCannedACLPrivate
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Try PutObjectAcl first (native ACL API)
	_, err := getClient().PutObjectAcl(ctx, &s3.PutObjectAclInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		ACL:    cannedACL,
	})
	if err != nil {
		// Fall back to copy-in-place with new ACL — wider provider support
		_, err = getClient().CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:            aws.String(bucket),
			CopySource:        aws.String(bucket + "/" + key),
			Key:               aws.String(key),
			ACL:               cannedACL,
			MetadataDirective: types.MetadataDirectiveCopy,
		})
		if err != nil {
			renderPartial(w, "acl-panel", map[string]interface{}{
				"Bucket":   bucket,
				"Key":      key,
				"ACLError": "Failed to update ACL: " + err.Error(),
				"CanWrite": true,
			})
			return
		}
	}

	isPublic := acl == "public-read"
	// Cache the result immediately — avoids a round-trip HEAD check
	aclStateCache.Store(aclCacheKey(bucket, key), isPublic)

	pubURL := ""
	if isPublic {
		pubURL = publicURL(bucket, key)
	}
	renderPartial(w, "acl-panel", map[string]interface{}{
		"Bucket":   bucket,
		"Key":      key,
		"IsPublic": isPublic,
		"PublicURL": pubURL,
		"CanWrite": true, // SetACL already verified write access above
	})
}

// publicURL builds the publicly-accessible URL for an object.
// If PUBLIC_FILES_HOST is set, uses the custom static server URL.
// Otherwise falls back to S3 virtual-hosted style: https://bucket.host/key.
func publicURL(bucket, key string) string {
	if host := os.Getenv("PUBLIC_FILES_HOST"); host != "" {
		host = strings.TrimRight(host, "/")
		if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
			host = "https://" + host
		}
		return fmt.Sprintf("%s/files/%s/%s", host, bucket, key)
	}

	session.mu.RLock()
	endpoint := session.Endpoint
	session.mu.RUnlock()

	if endpoint == "" {
		// AWS S3: virtual-hosted style
		return fmt.Sprintf("https://%s.s3.amazonaws.com/%s", bucket, key)
	}

	ep := normalizeEndpoint(strings.TrimRight(endpoint, "/"))
	// Split into scheme + host: "https://hel1.your-objectstorage.com" → "https", "hel1.your-objectstorage.com"
	if idx := strings.Index(ep, "://"); idx != -1 {
		scheme := ep[:idx]
		host := ep[idx+3:]
		// Virtual-hosted style: https://bucket.host/key
		return fmt.Sprintf("%s://%s.%s/%s", scheme, bucket, host, key)
	}
	// Fallback: path-style
	return fmt.Sprintf("%s/%s/%s", ep, bucket, key)
}

// PublicFileServe serves public objects at /files/{bucket}/{key...} without authentication.
// Returns 403 if the object is not publicly accessible.
func PublicFileServe(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		http.Error(w, "Storage not available", http.StatusServiceUnavailable)
		return
	}

	path := r.PathValue("path")
	// Split into bucket + key: "mybucket/folder/file.png" → "mybucket", "folder/file.png"
	idx := strings.Index(path, "/")
	if idx == -1 || idx == len(path)-1 {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	bucket := path[:idx]
	key := path[idx+1:]

	if !resolvePublicState(bucket, key) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := getClient().GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	defer result.Body.Close()

	if result.ContentType != nil {
		w.Header().Set("Content-Type", *result.ContentType)
	}
	if result.ContentLength != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*result.ContentLength, 10))
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000")

	io.Copy(w, result.Body)
}

// aclStateCache remembers the public/private state we last set or confirmed for each object.
// Key: "bucket\x00key", Value: bool (true = public).
// This ensures the copy-link button reflects the correct state immediately after SetACL,
// without relying on a slow or unreliable HEAD request.
var aclStateCache sync.Map

func aclCacheKey(bucket, key string) string { return bucket + "\x00" + key }

// resolvePublicState returns whether the object is publicly accessible.
// Checks the in-memory cache first (populated by SetACL), then falls back to
// an anonymous S3 HeadObject which is provider-agnostic and always reliable.
func resolvePublicState(bucket, key string) bool {
	ck := aclCacheKey(bucket, key)
	if v, ok := aclStateCache.Load(ck); ok {
		return v.(bool)
	}
	// Not in cache — probe via anonymous S3 HeadObject
	isPublic := checkPublicAccessS3(bucket, key)
	aclStateCache.Store(ck, isPublic) // cache for next call
	return isPublic
}

func SearchObjects(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}

	bucket := r.URL.Query().Get("bucket")
	query := r.URL.Query().Get("q")

	if bucket == "" || query == "" {
		renderPartial(w, "search-results", map[string]interface{}{})
		return
	}

	// Search across all objects
	var results []ObjectInfo
	var continuationToken *string

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := getClient().ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: continuationToken,
		})
		cancel()
		if err != nil {
			break
		}

		for _, obj := range out.Contents {
			if strings.Contains(strings.ToLower(aws.ToString(obj.Key)), strings.ToLower(query)) {
				results = append(results, ObjectInfo{
					Key:          aws.ToString(obj.Key),
					Size:         aws.ToInt64(obj.Size),
					LastModified: obj.LastModified,
				})
			}
		}

		if !aws.ToBool(out.IsTruncated) {
			break
		}
		continuationToken = out.NextContinuationToken

		if len(results) > 200 {
			break
		}
	}

	renderPartial(w, "search-results", map[string]interface{}{
		"Bucket":  bucket,
		"Results": results,
		"Query":   query,
	})
}

// ---- Helpers ----

func listBuckets() ([]types.Bucket, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := getClient().ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}
	return out.Buckets, nil
}

const objectPageSize = 20

// listObjectsPage fetches one page of objects (up to objectPageSize items).
// Returns the objects, the continuation token for the next page (empty if last page), and any error.
func listObjectsPage(bucket, prefix, token string) ([]ObjectInfo, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	input := &s3.ListObjectsV2Input{
		Bucket:    aws.String(bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int32(objectPageSize),
	}
	if token != "" {
		input.ContinuationToken = aws.String(token)
	}

	out, err := getClient().ListObjectsV2(ctx, input)
	if err != nil {
		return nil, "", fmt.Errorf("failed to list objects: %w", err)
	}

	var objects []ObjectInfo
	for _, cp := range out.CommonPrefixes {
		objects = append(objects, ObjectInfo{
			Key:      aws.ToString(cp.Prefix),
			IsFolder: true,
		})
	}
	for _, obj := range out.Contents {
		key := aws.ToString(obj.Key)
		if key == prefix {
			continue
		}
		objects = append(objects, ObjectInfo{
			Key:          key,
			Size:         aws.ToInt64(obj.Size),
			LastModified: obj.LastModified,
			ETag:         strings.Trim(aws.ToString(obj.ETag), `"`),
		})
	}

	nextToken := ""
	if aws.ToBool(out.IsTruncated) {
		nextToken = aws.ToString(out.NextContinuationToken)
	}
	return objects, nextToken, nil
}

func listObjects(bucket, prefix string) ([]ObjectInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var objects []ObjectInfo
	var continuationToken *string

	for {
		out, err := getClient().ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		// Add common prefixes (folders)
		for _, cp := range out.CommonPrefixes {
			objects = append(objects, ObjectInfo{
				Key:      aws.ToString(cp.Prefix),
				IsFolder: true,
			})
		}

		// Add objects
		for _, obj := range out.Contents {
			key := aws.ToString(obj.Key)
			if key == prefix {
				continue // skip the "folder itself" placeholder
			}
			objects = append(objects, ObjectInfo{
				Key:          key,
				Size:         aws.ToInt64(obj.Size),
				LastModified: obj.LastModified,
				ETag:         strings.Trim(aws.ToString(obj.ETag), `"`),
			})
		}

		if !aws.ToBool(out.IsTruncated) {
			break
		}
		continuationToken = out.NextContinuationToken
	}

	return objects, nil
}

func deleteFolder(ctx context.Context, bucket, prefix string) error {
	var toDelete []types.ObjectIdentifier
	var continuationToken *string

	for {
		out, err := getClient().ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return fmt.Errorf("failed to list objects for deletion: %w", err)
		}

		for _, obj := range out.Contents {
			toDelete = append(toDelete, types.ObjectIdentifier{Key: obj.Key})
		}

		if !aws.ToBool(out.IsTruncated) {
			break
		}
		continuationToken = out.NextContinuationToken
	}

	if len(toDelete) == 0 {
		return nil
	}

	// Delete in batches of 1000
	for i := 0; i < len(toDelete); i += 1000 {
		end := i + 1000
		if end > len(toDelete) {
			end = len(toDelete)
		}
		batch := toDelete[i:end]
		_, err := getClient().DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: batch},
		})
		if err != nil {
			return fmt.Errorf("failed to delete objects: %w", err)
		}
	}

	return nil
}

func objectStats(objects []ObjectInfo) (totalSize int64, fileCount int) {
	for _, o := range objects {
		if !o.IsFolder {
			totalSize += o.Size
			fileCount++
		}
	}
	return
}

func sortObjects(objects []ObjectInfo, by, dir string) {
	sort.SliceStable(objects, func(i, j int) bool {
		// Folders always first
		if objects[i].IsFolder != objects[j].IsFolder {
			return objects[i].IsFolder
		}

		var less bool
		switch by {
		case "size":
			less = objects[i].Size < objects[j].Size
		case "date":
			if objects[i].LastModified == nil {
				less = true
			} else if objects[j].LastModified == nil {
				less = false
			} else {
				less = objects[i].LastModified.Before(*objects[j].LastModified)
			}
		default: // name
			less = strings.ToLower(objects[i].Key) < strings.ToLower(objects[j].Key)
		}

		if dir == "desc" {
			return !less
		}
		return less
	})
}

func renderTemplate(w http.ResponseWriter, name string, data interface{}) {
	t := template.Must(template.New("layout.html").Funcs(funcMap()).ParseFiles(
		"templates/layout.html",
		"templates/index.html",
		"templates/partials/bucket-list.html",
		"templates/partials/object-list.html",
		"templates/partials/object-rows.html",
		"templates/partials/search-results.html",
	))
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func renderPartial(w http.ResponseWriter, name string, data interface{}) {
	files := []string{"templates/partials/" + name + ".html"}
	// object-list.html uses {{template "object-rows.html" .}} so both must be parsed together
	if name == "object-list" {
		files = append(files, "templates/partials/object-rows.html")
	}
	t, err := template.New(name + ".html").Funcs(funcMap()).ParseFiles(files...)
	if err != nil {
		log.Printf("partial template parse error (%s): %v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, name+".html", data); err != nil {
		log.Printf("partial template error (%s): %v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func renderError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<div class="alert-error">%s</div>`, template.HTMLEscapeString(msg))
}
