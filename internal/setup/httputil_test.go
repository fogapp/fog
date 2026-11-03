package setup

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newMultipart(t *testing.T, buf *bytes.Buffer, kit []byte, passphrase string) string {
	t.Helper()
	mw := multipart.NewWriter(buf)
	fw, err := mw.CreateFormFile("kit", "kit.enc")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(kit)
	mw.WriteField("passphrase", passphrase)
	mw.Close()
	return mw.FormDataContentType()
}

func newTestRequest(t *testing.T, method, path string, body io.Reader, contentType string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func doHandler(a *Agent, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.Handler(nil).ServeHTTP(rec, req)
	return rec
}
