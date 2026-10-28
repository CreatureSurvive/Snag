package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Optional S3-compatible storage (AWS S3, Cloudflare R2, Backblaze B2, MinIO...) for screenshots,
// recordings and other binary attachments. Reports, issues and text attachments (logs, crash
// logs) stay on disk, so search and symbolication keep working without S3 round trips.

// S3Config is set from SNAG_S3_*.
type S3Config struct {
	Endpoint  string // https://s3.us-east-1.amazonaws.com, https://<account>.r2.cloudflarestorage.com, http://minio:9000
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	Prefix    string // key prefix inside the bucket, e.g. "snag/"
}

// S3 is a minimal S3 client: path-style requests signed with SigV4 (UNSIGNED-PAYLOAD).
type S3 struct {
	cfg    S3Config
	client *http.Client
	now    func() time.Time
}

func NewS3(cfg S3Config) (*S3, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("SNAG_S3_ENDPOINT %q isn't an http(s) URL", cfg.Endpoint)
	}
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("S3 needs SNAG_S3_BUCKET, SNAG_S3_ACCESS_KEY_ID and SNAG_S3_SECRET_ACCESS_KEY")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	return &S3{cfg: cfg, client: &http.Client{Timeout: 5 * time.Minute}, now: time.Now}, nil
}

func (c *S3) key(reportID, name string) string {
	return c.cfg.Prefix + "reports/" + strings.ToLower(reportID) + "/" + name
}

// s3Escape encodes a key the way SigV4 canonical URIs expect: everything but unreserved
// characters and "/".
func s3Escape(key string) string {
	var b strings.Builder
	for _, c := range []byte(key) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// sign adds SigV4 headers to req.
func (c *S3) sign(req *http.Request) {
	now := c.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "x-amz-") {
			headers[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, k := range names {
		canonicalHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	query := req.URL.Query()
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var cq []string
	for _, k := range keys {
		for _, v := range query[k] {
			cq = append(cq, s3Escape(k)+"="+strings.ReplaceAll(s3Escape(v), "/", "%2F"))
		}
	}
	canonical := strings.Join([]string{req.Method, req.URL.EscapedPath(), strings.Join(cq, "&"), canonicalHeaders.String(), signed, "UNSIGNED-PAYLOAD"}, "\n")
	scope := day + "/" + c.cfg.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), day)
	key = hmacSHA256(key, c.cfg.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.cfg.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+signature)
}

func (c *S3) request(ctx context.Context, method, key string, body io.Reader, size int64) (*http.Request, error) {
	u, _ := url.Parse(c.cfg.Endpoint)
	u.Path = "/" + c.cfg.Bucket + "/" + key
	u.RawPath = "/" + s3Escape(c.cfg.Bucket) + "/" + s3Escape(key)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	return req, nil
}

func s3Error(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("S3 %s: %s", resp.Status, strings.TrimSpace(string(data)))
}

func (c *S3) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	req, err := c.request(ctx, http.MethodPut, key, body, size)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	c.sign(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return s3Error(resp)
	}
	return nil
}

// Get fetches an object; byteRange is an HTTP Range header value or "".
func (c *S3) Get(ctx context.Context, key, byteRange string) (*http.Response, error) {
	req, err := c.request(ctx, http.MethodGet, key, nil, 0)
	if err != nil {
		return nil, err
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	c.sign(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close()
		return nil, s3Error(resp)
	}
	return resp, nil
}

func (c *S3) Delete(ctx context.Context, key string) error {
	req, err := c.request(ctx, http.MethodDelete, key, nil, 0)
	if err != nil {
		return err
	}
	c.sign(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return s3Error(resp)
	}
	return nil
}

// offloadable is what goes to S3: everything but text, which stays local for search.
func offloadable(f StoredFile) bool { return !f.Remote && !f.IsText() }

// Offload moves a stored report's binary attachments to S3, then removes the local copies.
func (s *Server) Offload(ctx context.Context, id string) error {
	if s.s3 == nil {
		return nil
	}
	rec, err := s.store.Get(id)
	if err != nil {
		return err
	}
	for _, f := range rec.Files {
		if !offloadable(f) {
			continue
		}
		path := filepath.Join(s.store.reportDir(rec.ID), f.Name)
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		err = s.s3.Put(ctx, s.s3.key(rec.ID, f.Name), file, f.Size, f.ContentType)
		file.Close()
		if err != nil {
			return fmt.Errorf("uploading %s: %w", f.Filename, err)
		}
		name := f.Name
		if _, err := s.store.Update(rec.ID, func(r *Record) {
			for i := range r.Files {
				if r.Files[i].Name == name {
					r.Files[i].Remote = true
				}
			}
		}); err != nil {
			return err
		}
		_ = os.Remove(path)
	}
	return nil
}

// OffloadPending sweeps reports whose media is still local (after an outage, or when S3 was
// turned on with existing data).
func (s *Server) OffloadPending(ctx context.Context) (moved int) {
	if s.s3 == nil {
		return 0
	}
	for _, rec := range s.store.All() {
		pending := false
		for _, f := range rec.Files {
			pending = pending || offloadable(f)
		}
		if !pending {
			continue
		}
		if err := s.Offload(ctx, rec.ID); err != nil {
			s.log.Warn("moving attachments to S3", "report", rec.ID, "err", err)
			continue
		}
		moved++
	}
	return moved
}

// openFile opens an attachment wherever it lives. Local copies win (a restored export holds
// its files locally even when the record says they were in S3).
func (s *Server) openFile(ctx context.Context, id string, f StoredFile) (io.ReadCloser, error) {
	path := filepath.Join(s.store.reportDir(id), f.Name)
	if file, err := os.Open(path); err == nil || !f.Remote || s.s3 == nil {
		return file, err
	}
	resp, err := s.s3.Get(ctx, s.s3.key(id, f.Name), "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// serveRemote proxies an S3 object, passing Range through so videos can seek.
func (s *Server) serveRemote(w http.ResponseWriter, r *http.Request, id string, f StoredFile) {
	resp, err := s.s3.Get(r.Context(), s.s3.key(id, f.Name), r.Header.Get("Range"))
	if err != nil {
		s.log.Warn("reading attachment from S3", "report", id, "err", err)
		http.Error(w, "attachment unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// deleteRemote removes a report's S3 objects (best effort).
func (s *Server) deleteRemote(rec *Record) {
	if s.s3 == nil {
		return
	}
	for _, f := range rec.Files {
		if f.Remote {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := s.s3.Delete(ctx, s.s3.key(rec.ID, f.Name)); err != nil {
				s.log.Warn("deleting attachment from S3", "report", rec.ID, "err", err)
			}
			cancel()
		}
	}
}
