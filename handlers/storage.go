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
)

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
	customResolver := aws.EndpointResolverWithOptionsFunc(func(service, reg string, options ...interface{}) (aws.Endpoint, error) {
		if endpoint != "" {
			return aws.Endpoint{
				URL:               endpoint,
				HostnameImmutable: true,
				SigningRegion:     region,
			}, nil
		}
		return aws.Endpoint{}, &aws.EndpointNotFoundError{}
	})

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		awsconfig.WithEndpointResolverWithOptions(customResolver),
	)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true }), nil
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

func Index(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"Connected":      isConnected(),
		"DefaultEndpoint": os.Getenv("ENDPOINT"),
		"DefaultRegion":   func() string { r := os.Getenv("REGION"); if r == "" { return "us-east-1" }; return r }(),
	}

	if isConnected() {
		buckets, err := listBuckets()
		if err == nil {
			data["Buckets"] = buckets
		}
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
		"Buckets": buckets,
	})
}

func CreateBucket(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
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
		"Buckets":  buckets,
		"Toast":    "Bucket '" + name + "' created successfully",
		"ToastOK":  true,
	})
}

func DeleteBucket(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
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
		"Buckets":  buckets,
		"Toast":    "Bucket '" + bucket + "' deleted",
		"ToastOK":  true,
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

	objects, err := listObjects(bucket, prefix)
	if err != nil {
		renderError(w, err.Error())
		return
	}

	// Filter by search
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

	// Sort
	sortObjects(objects, sortBy, sortDir)

	// Get bucket stats
	var totalSize int64
	var fileCount int
	for _, o := range objects {
		if !o.IsFolder {
			totalSize += o.Size
			fileCount++
		}
	}

	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":     bucket,
		"Prefix":     prefix,
		"Objects":    objects,
		"Search":     search,
		"SortBy":     sortBy,
		"SortDir":    sortDir,
		"TotalSize":  totalSize,
		"FileCount":  fileCount,
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

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		_, err = getClient().PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(bucket),
			Key:         aws.String(key),
			Body:        f,
			ContentType: aws.String(contentType),
		})
		cancel()
		if err != nil {
			errs = append(errs, fh.Filename+": "+err.Error())
			continue
		}
		uploaded = append(uploaded, fh.Filename)
	}

	msg := fmt.Sprintf("Uploaded %d file(s)", len(uploaded))
	if len(errs) > 0 {
		msg += fmt.Sprintf(", %d failed: %s", len(errs), strings.Join(errs, "; "))
	}

	objects, _ := listObjects(bucket, prefix)
	sortObjects(objects, "", "")

	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":    bucket,
		"Prefix":    prefix,
		"Objects":   objects,
		"Toast":     msg,
		"ToastOK":   len(errs) == 0,
	})
}

func DownloadObject(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		http.Error(w, "Not connected", http.StatusUnauthorized)
		return
	}

	bucket := r.PathValue("bucket")
	key := r.PathValue("key")

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
	}

	objects, _ := listObjects(bucket, prefix)
	sortObjects(objects, "", "")

	name := filepath.Base(strings.TrimSuffix(key, "/"))
	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":   bucket,
		"Prefix":   prefix,
		"Objects":  objects,
		"Toast":    "'" + name + "' deleted successfully",
		"ToastOK":  true,
	})
}

func PresignObject(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		renderError(w, "Not connected")
		return
	}

	bucket := r.PathValue("bucket")
	key := r.PathValue("key")
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

	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":  bucket,
		"Prefix":  prefix,
		"Objects": objects,
		"Toast":   "Folder '" + folderName + "' created",
		"ToastOK": true,
	})
}

func PreviewObject(w http.ResponseWriter, r *http.Request) {
	if !isConnected() {
		http.Error(w, "Not connected", http.StatusUnauthorized)
		return
	}

	bucket := r.PathValue("bucket")
	key := r.PathValue("key")

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

	// If rename (source != dest prefix area), delete original
	if r.FormValue("move") == "true" {
		_, _ = getClient().DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(sourceKey),
		})
	}

	objects, _ := listObjects(bucket, prefix)
	sortObjects(objects, "", "")

	renderPartial(w, "object-list", map[string]interface{}{
		"Bucket":  bucket,
		"Prefix":  prefix,
		"Objects": objects,
		"Toast":   "Operation completed successfully",
		"ToastOK": true,
	})
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
		"templates/partials/search-results.html",
	))
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func renderPartial(w http.ResponseWriter, name string, data interface{}) {
	t := template.Must(template.New(name+".html").Funcs(funcMap()).ParseFiles(
		"templates/partials/"+name+".html",
	))
	if err := t.ExecuteTemplate(w, name+".html", data); err != nil {
		log.Printf("partial template error (%s): %v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func renderError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<div class="alert-error">%s</div>`, template.HTMLEscapeString(msg))
}
