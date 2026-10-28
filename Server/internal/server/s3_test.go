package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Runs against a real S3-compatible server when SNAG_TEST_S3_ENDPOINT, _ACCESS_KEY and
// _SECRET_KEY are set, e.g. MinIO:
//
//	docker run -d -p 9000:9000 -e MINIO_ROOT_USER=… -e MINIO_ROOT_PASSWORD=… minio/minio server /data
func TestS3Storage(t *testing.T) {
	endpoint := os.Getenv("SNAG_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set SNAG_TEST_S3_ENDPOINT, SNAG_TEST_S3_ACCESS_KEY and SNAG_TEST_S3_SECRET_KEY")
	}
	bucket := "snag-test-" + strings.ToLower(newID())
	s3cfg := S3Config{Endpoint: endpoint, Bucket: bucket, AccessKey: os.Getenv("SNAG_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("SNAG_TEST_S3_SECRET_KEY"), Prefix: "attachments/"}
	client, err := NewS3(s3cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Create the bucket.
	req, _ := http.NewRequest(http.MethodPut, strings.TrimRight(endpoint, "/")+"/"+bucket, nil)
	client.sign(req)
	if resp, err := client.client.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("create bucket: %v %v", err, resp)
	}

	cfg := testConfig(t)
	cfg.S3 = &s3cfg
	srv, store := newTestServer(t, cfg)
	if rr := post(t, srv, sampleUpload(id1), testKey); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d", rr.Code)
	}
	id := strings.ToLower(id1)
	deadline := time.Now().Add(10 * time.Second)
	var rec *Record
	for time.Now().Before(deadline) {
		rec, _ = store.Get(id)
		if rec.Files[0].Remote {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	shot, log := rec.Files[0], rec.Files[1]
	if !shot.Remote || log.Remote {
		t.Fatalf("screenshot should move to S3 and the log stay local: %+v", rec.Files)
	}
	if _, err := os.Stat(filepath.Join(store.reportDir(id), shot.Name)); !os.IsNotExist(err) {
		t.Fatal("local copy of the screenshot still exists")
	}

	// The dashboard serves it from S3, including ranges.
	rr := adminGet(srv, "/reports/"+id+"/files/"+shot.Name)
	if rr.Code != 200 || !strings.HasPrefix(rr.Body.String(), "\x89PNG") || rr.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("serve: %d %q", rr.Code, rr.Body.String()[:8])
	}
	req = httptest.NewRequest("GET", "/reports/"+id+"/files/"+shot.Name, nil)
	req.SetBasicAuth("admin", "pw-for-tests")
	req.Header.Set("Range", "bytes=1-3")
	ranged := httptest.NewRecorder()
	srv.ServeHTTP(ranged, req)
	if ranged.Code != http.StatusPartialContent || ranged.Body.String() != "PNG" {
		t.Fatalf("range: %d %q", ranged.Code, ranged.Body.String())
	}

	// Exports include it.
	export := adminPost(t, srv, "/p/gelo/export", nil)
	if !strings.Contains(strings.Join(archiveNames(t, export.Body.Bytes()), ","), "reports/"+id+"/"+shot.Name) {
		t.Error("export lacks the S3 attachment")
	}

	// Deleting the report deletes the object.
	adminPost(t, srv, "/p/gelo/reports/"+id+"/delete", nil)
	deadline = time.Now().Add(10 * time.Second)
	for {
		_, err := client.Get(context.Background(), client.key(id, shot.Name), "")
		if err != nil && strings.Contains(err.Error(), "404") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("object not deleted: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
