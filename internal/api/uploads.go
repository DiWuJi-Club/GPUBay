package api

import (
	"errors"
	"io"
	"net/http"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/storage"
)

const uploadPath = "/api/v1/uploads"

// handleUpload 接收 multipart/form-data 中名为 file 的文件（营业执照）。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, storage.MaxUploadSize+1<<20) // 额外 1MB 留给 multipart 头部
	mr, err := r.MultipartReader()
	if err != nil {
		s.writeError(w, r, apperr.BadRequest("请使用 multipart/form-data 上传，文件字段名为 file"))
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			s.writeError(w, r, apperr.BadRequest("缺少文件字段 file"))
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, r, storage.ErrFileTooLarge)
			return
		}
		if err != nil {
			s.writeError(w, r, apperr.BadRequest("上传内容格式不正确"))
			return
		}
		if part.FormName() != "file" {
			continue
		}
		up, err := s.Uploads.Save(r.Context(), currentUser(r).ID, storage.PurposeLicense, part)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, up)
		return
	}
}
