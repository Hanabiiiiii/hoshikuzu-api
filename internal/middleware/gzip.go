package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

var gzipPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

type gzipWriter struct {
	gin.ResponseWriter
	writer   *gzip.Writer
	started  bool
	compress bool
}

func (w *gzipWriter) WriteHeader(code int) {
	if !w.started {
		w.started = true
		if bodyAllowed(code) {
			w.compress = true
			header := w.Header()
			header.Del("Content-Length")
			header.Set("Content-Encoding", "gzip")
			header.Add("Vary", "Accept-Encoding")
			w.writer.Reset(w.ResponseWriter)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipWriter) WriteHeaderNow() {
	w.ResponseWriter.WriteHeaderNow()
}

func (w *gzipWriter) Write(data []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if !w.compress {
		return w.ResponseWriter.Write(data)
	}
	return w.writer.Write(data)
}

func (w *gzipWriter) WriteString(s string) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if !w.compress {
		return w.ResponseWriter.WriteString(s)
	}
	return w.writer.Write([]byte(s))
}

// Gzip 只压缩已知的文本响应（页面 / 静态资源 / JSON 接口）。
// 图片、ZIP 下载等二进制响应直接跳过，避免白费 CPU。
func Gzip() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !shouldCompress(c.Request.URL.Path) || !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}

		writer := gzipPool.Get().(*gzip.Writer)
		gw := &gzipWriter{ResponseWriter: c.Writer, writer: writer}
		c.Writer = gw

		c.Next()

		if gw.compress {
			_ = writer.Close()
		}
		gzipPool.Put(writer)
	}
}

func shouldCompress(path string) bool {
	switch path {
	case "/", "/app.js", "/style.css", "/healthz":
		return true
	}
	return strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/api/images/")
}

func bodyAllowed(status int) bool {
	if status >= 100 && status <= 199 {
		return false
	}
	return status != http.StatusNoContent && status != http.StatusNotModified
}
