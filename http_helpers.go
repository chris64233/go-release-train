package releasetrain

import (
	"bytes"
	"io"
	"net/http"
)

// readBody 读取请求体并把它放回 r.Body，使处理器可再次读取。
func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

// recordingWriter 缓存处理器写出的状态码与响应体，供幂等层记录。
type recordingWriter struct {
	orig   http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func newRecordingWriter(orig http.ResponseWriter) *recordingWriter {
	return &recordingWriter{orig: orig, status: http.StatusOK}
}

func (w *recordingWriter) Header() http.Header { return w.orig.Header() }

func (w *recordingWriter) WriteHeader(status int) {
	w.status = status
	w.orig.WriteHeader(status)
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	return w.orig.Write(p)
}

// 保证实现 http.Flusher 等可选接口的透传（若底层支持）。
var _ http.ResponseWriter = (*recordingWriter)(nil)
