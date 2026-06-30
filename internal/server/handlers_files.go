package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"xraytool/internal/appconfig"
	"xraytool/internal/safeio"
)

func isPathAllowed(cfg *appconfig.Config, path string) bool {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		realPath, err = filepath.Abs(path)
		if err != nil {
			return false
		}
	}

	for _, dir := range cfg.Server.AllowedDirs {
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			realDir, err = filepath.Abs(dir)
			if err != nil {
				continue
			}
		}

		rel, err := filepath.Rel(realDir, realPath)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

func (r *Router) handleUpdateLinks(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseMultipartForm(10 << 20); err != nil {
		r.log.Warn("Form Parse Error", "err", err, "ip", getClientIP(req))
		writeError(w, http.StatusBadRequest, "form parse error")
		return
	}

	destPath := req.FormValue("path")
	if destPath == "" {
		r.log.Warn("Missing 'path' field", "ip", getClientIP(req))
		writeError(w, http.StatusBadRequest, "missing path")
		return
	}

	if !isPathAllowed(r.cfg, destPath) {
		r.log.Warn("Path Not Allowed", "path", destPath, "ip", getClientIP(req))
		writeError(w, http.StatusForbidden, "path not allowed")
		return
	}

	const maxUploadBytes = 10 * 1024 * 1024 // 10 MB
	req.Body = http.MaxBytesReader(w, req.Body, maxUploadBytes)
	file, _, err := req.FormFile("file")
	if err != nil {
		r.log.Warn("Missing 'file' field", "ip", getClientIP(req))
		writeError(w, http.StatusBadRequest, "missing file")
		return
	}
	defer file.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		r.log.Error("Ошибка создания директорий", "err", err)
		writeError(w, http.StatusInternalServerError, "disk error")
		return
	}

	dst, err := os.Create(destPath)
	if err != nil {
		r.log.Error("Ошибка записи файла", "err", err)
		writeError(w, http.StatusInternalServerError, "disk error")
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, file); err != nil {
		r.log.Error("Ошибка записи тела файла", "err", err)
		writeError(w, http.StatusInternalServerError, "write error")
		return
	}

	go func() {
		if r.cfg != nil {
			r.dispatcher.Dispatch("file.uploaded", map[string]interface{}{
				"path": destPath,
			}, nil)
		}
	}()

	r.log.Info("Файл успешно загружен (вебхук отправлен)", "path", destPath, "ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]string{"status": "success", "message": "file saved & webhook dispatched"})
}

func (r *Router) handleUpload(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseMultipartForm(10 << 20); err != nil {
		r.log.Warn("Form Parse Error", "err", err, "ip", getClientIP(req))
		writeError(w, http.StatusBadRequest, "form parse error")
		return
	}

	destPath := req.FormValue("path")
	if destPath == "" {
		r.log.Warn("Missing 'path' field", "ip", getClientIP(req))
		writeError(w, http.StatusBadRequest, "missing path")
		return
	}

	if !isPathAllowed(r.cfg, destPath) {
		r.log.Warn("Path Not Allowed", "path", destPath, "ip", getClientIP(req))
		writeError(w, http.StatusForbidden, "path not allowed")
		return
	}

	r.log.Info("Incoming upload request", "ip", getClientIP(req), "path", destPath)

	const maxUploadBytes2 = 10 * 1024 * 1024 // 10 MB
	req.Body = http.MaxBytesReader(w, req.Body, maxUploadBytes2)
	file, _, err := req.FormFile("file")
	if err != nil {
		r.log.Warn("Missing 'file' field", "ip", getClientIP(req))
		writeError(w, http.StatusBadRequest, "missing file")
		return
	}
	defer file.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		r.log.Error("Ошибка создания директорий", "err", err)
		writeError(w, http.StatusInternalServerError, "disk error")
		return
	}

	if strings.HasSuffix(destPath, ".exe") || strings.HasSuffix(destPath, ".yaml") || strings.HasSuffix(destPath, ".yml") {
		r.log.Warn("Attempted to upload critical file extension", "path", destPath)
		writeError(w, http.StatusForbidden, "forbidden file extension")
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		r.log.Error("Ошибка чтения тела файла", "err", err)
		writeError(w, http.StatusInternalServerError, "read error")
		return
	}

	if err := safeio.WriteToFile(destPath, data, 0644); err != nil {
		r.log.Error("Ошибка записи файла", "err", err)
		writeError(w, http.StatusInternalServerError, "disk error")
		return
	}

	r.log.Info("Файл успешно загружен", "path", destPath, "ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]string{"status": "success", "message": "file saved"})
}

func (r *Router) handleDownload(w http.ResponseWriter, req *http.Request) {
	srcPath := req.URL.Query().Get("path")
	if srcPath == "" {
		r.log.Warn("Missing 'path' parameter in download", "ip", getClientIP(req))
		writeError(w, http.StatusBadRequest, "missing path")
		return
	}

	if !isPathAllowed(r.cfg, srcPath) {
		r.log.Warn("Attempted to download from unauthorized directory", "path", srcPath, "ip", getClientIP(req))
		writeError(w, http.StatusForbidden, "path not allowed")
		return
	}

	r.log.Info("Incoming download request", "ip", getClientIP(req), "path", srcPath)

	info, err := os.Stat(srcPath)
	if os.IsNotExist(err) || info.IsDir() {
		r.log.Warn("File Not Found", "path", srcPath, "ip", getClientIP(req))
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	r.log.Info("Отправка файла", "path", srcPath, "ip", getClientIP(req))
	http.ServeFile(w, req, srcPath)
}
