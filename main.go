// static-files: a minimal single-bucket S3 explorer.
//
// It lists, uploads, and hands out the public URL for objects in ONE bucket.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

//go:embed static/*
var staticFiles embed.FS

type server struct {
	s3        *s3.Client
	uploader  *manager.Uploader
	bucket    string
	rootPfx   string // everything is confined under this prefix ("" = whole bucket)
	publicURL string // base for public links, no trailing slash
	maxUpload int64
}

func main() {
	cfg := loadEnv()

	awsCfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(cfg.region))
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	client := s3.NewFromConfig(awsCfg)

	srv := &server{
		s3:        client,
		uploader:  manager.NewUploader(client),
		bucket:    cfg.bucket,
		rootPfx:   cfg.rootPrefix,
		publicURL: cfg.publicURL,
		maxUpload: cfg.maxUploadMB << 20,
	}

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Printf("healthz write: %v", err)
		}
	})
	mux.HandleFunc("GET /api/list", srv.handleList)
	mux.HandleFunc("POST /api/upload", srv.handleUpload)
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Minute, // large uploads stream in
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("static-files listening on %s (bucket=%s, root=%q, publicURL=%s)",
		cfg.listen, cfg.bucket, cfg.rootPrefix, cfg.publicURL)

	// Serve, but shut down gracefully on SIGINT/SIGTERM so in-flight
	// uploads get a chance to finish before the process exits.
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case sig := <-sigCh:
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}

type envConfig struct {
	bucket      string
	region      string
	listen      string
	rootPrefix  string
	publicURL   string
	maxUploadMB int64
}

func loadEnv() envConfig {
	bucket := mustEnv("BUCKET")
	region := envOr("AWS_REGION", "us-east-1")
	c := envConfig{
		bucket:      bucket,
		region:      region,
		listen:      envOr("LISTEN_ADDR", ":8334"),
		rootPrefix:  normPrefix(os.Getenv("ROOT_PREFIX")),
		maxUploadMB: envInt("MAX_UPLOAD_MB", 512),
	}
	// Default public base = virtual-hosted style. Override with PUBLIC_BASE_URL
	// (e.g. a CloudFront domain) if the bucket is fronted by a CDN.
	c.publicURL = strings.TrimRight(
		envOr("PUBLIC_BASE_URL", fmt.Sprintf("https://%s.s3.%s.amazonaws.com", bucket, region)),
		"/")
	return c
}

type object struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	URL      string `json:"url"`
}

type listResponse struct {
	Prefix  string   `json:"prefix"`
	Folders []string `json:"folders"`
	Objects []object `json:"objects"`
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	prefix, err := s.resolvePrefix(r.URL.Query().Get("prefix"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	resp := listResponse{Prefix: s.displayPrefix(prefix), Folders: []string{}, Objects: []object{}}
	p := s3.NewListObjectsV2Paginator(s.s3, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			httpError(w, http.StatusBadGateway, "list: "+err.Error())
			return
		}
		for _, cp := range page.CommonPrefixes {
			resp.Folders = append(resp.Folders, s.displayPrefix(aws.ToString(cp.Prefix)))
		}
		for _, o := range page.Contents {
			key := aws.ToString(o.Key)
			if strings.HasSuffix(key, "/") { // skip the folder placeholder object
				continue
			}
			resp.Objects = append(resp.Objects, object{
				Key:      key,
				Name:     path.Base(key),
				Size:     aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified).UTC().Format(time.RFC3339),
				URL:      s.publicURL + "/" + key,
			})
		}
	}
	writeJSON(w, resp)
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	prefix, err := s.resolvePrefix(r.URL.Query().Get("prefix"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		httpError(w, http.StatusBadRequest, "no files (field 'files')")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	uploaded := []object{}
	for _, fh := range files {
		name := baseName(fh.Filename) // strip any client path
		if name == "" || name == "." || name == "/" {
			httpError(w, http.StatusBadRequest, "bad filename")
			return
		}
		key := prefix + name
		f, err := fh.Open()
		if err != nil {
			httpError(w, http.StatusBadRequest, "open: "+err.Error())
			return
		}
		_, err = s.uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(s.bucket),
			Key:         aws.String(key),
			Body:        f,
			ContentType: aws.String(contentType(fh.Header.Get("Content-Type"), name)),
		})
		f.Close()
		if err != nil {
			httpError(w, http.StatusBadGateway, "upload "+name+": "+err.Error())
			return
		}
		uploaded = append(uploaded, object{
			Key:      key,
			Name:     name,
			Size:     fh.Size,
			Modified: time.Now().UTC().Format(time.RFC3339),
			URL:      s.publicURL + "/" + key,
		})
	}
	writeJSON(w, map[string]any{"uploaded": uploaded})
}

// resolvePrefix cleans a user-supplied prefix and confines it under rootPfx.
// Returns the full S3 prefix (rootPfx + user part), always ending in "/" (or "").
func (s *server) resolvePrefix(user string) (string, error) {
	user = strings.TrimPrefix(user, "/")
	// path.Clean on an absolute path collapses "." and ".." and can never
	// produce a result that escapes "/", so traversal above rootPfx is
	// impossible here (e.g. "../../x" -> "/x"). No leftover ".." to reject.
	rel := strings.TrimPrefix(path.Clean("/"+user), "/")
	if rel == "" {
		return s.rootPfx, nil
	}
	full := s.rootPfx + rel
	if !strings.HasSuffix(full, "/") {
		full += "/"
	}
	return full, nil
}

// displayPrefix strips rootPfx so the UI never sees the confinement root.
func (s *server) displayPrefix(full string) string {
	return strings.TrimPrefix(full, s.rootPfx)
}

func contentType(fromHeader, name string) string {
	if fromHeader != "" && fromHeader != "application/octet-stream" {
		return fromHeader
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".woff2":
		return "font/woff2"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

// baseName returns the last path element of a client-supplied filename,
// treating both / and \ as separators.
func baseName(name string) string {
	return path.Base(strings.ReplaceAll(name, "\\", "/"))
}

func normPrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing required env %s", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode json: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": msg}); err != nil {
		log.Printf("encode error json: %v", err)
	}
}

// statusRecorder captures the response status code for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}
