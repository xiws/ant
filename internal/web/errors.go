package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/xiws/ant/internal/model"
)

// apiErr 是 Web 层的错误；class 使用 not_found/bad_request/too_large/
// unavailable/internal，只有来自 session/config 的 *model.Error 才保留原 class。
type apiErr struct {
	class   string
	code    string
	message string
	hint    string
	details map[string]any
	status  int
}

func (e *apiErr) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.class + "/" + e.code + ": " + e.message
}

func newAPIErr(class, code, message string) *apiErr {
	return &apiErr{class: class, code: code, message: message}
}

// errFromModel 把任意 error 转换为 apiErr，保留 model.Error 的分类与细码。
func errFromModel(err error) *apiErr {
	if err == nil {
		return nil
	}
	var ae *apiErr
	if errors.As(err, &ae) {
		return ae
	}
	var me *model.Error
	if errors.As(err, &me) {
		out := &apiErr{
			class:   string(me.Class),
			code:    me.Code,
			message: me.Message,
			hint:    me.Hint,
			details: me.Details,
			status:  statusForCode(me.Code),
		}
		if out.message == "" {
			out.message = me.Error()
		}
		if out.status == 0 {
			switch me.Class {
			case model.ErrConfigInvalid:
				out.status = http.StatusBadRequest
			default:
				out.status = http.StatusInternalServerError
			}
		}
		return out
	}
	return &apiErr{class: "internal", code: "internal", message: err.Error(), status: http.StatusInternalServerError}
}

func statusForCode(code string) int {
	switch {
	case code == "session_not_found", code == "artifact_not_found", code == "file_not_found", code == "no_sessions":
		return http.StatusNotFound
	case code == "session_too_large", code == "artifact_too_large", code == "file_too_large":
		return http.StatusRequestEntityTooLarge
	}
	if strings.HasSuffix(code, "_too_large") {
		return http.StatusRequestEntityTooLarge
	}
	return 0
}

func (e *apiErr) httpStatus() int {
	if e == nil {
		return http.StatusInternalServerError
	}
	if e.status != 0 {
		return e.status
	}
	switch e.class {
	case "not_found":
		return http.StatusNotFound
	case "bad_request":
		return http.StatusBadRequest
	case "too_large":
		return http.StatusRequestEntityTooLarge
	case "unavailable":
		return http.StatusServiceUnavailable
	}
	if strings.HasSuffix(e.code, "_too_large") {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusInternalServerError
}

// writeJSON 写 JSON 响应；公共头由中间件统一附加。
func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"class":"internal","code":"encode_failed","message":"响应序列化失败"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeAPIError 写统一错误 JSON。
func writeAPIError(w http.ResponseWriter, err error) {
	ae := errFromModel(err)
	if ae == nil {
		ae = newAPIErr("internal", "internal", "未知错误")
	}
	body := APIError{Error: APIErrorBody{
		Class:   ae.class,
		Code:    ae.code,
		Message: ae.message,
		Hint:    ae.hint,
		Details: ae.details,
	}}
	writeJSON(w, ae.httpStatus(), body)
}

// badRequest 构造 400。
func badRequest(code, message string) *apiErr {
	return &apiErr{class: "bad_request", code: code, message: message, status: http.StatusBadRequest}
}

// notFound 构造 404。
func notFound(code, message string) *apiErr {
	return &apiErr{class: "not_found", code: code, message: message, status: http.StatusNotFound}
}

// unavailable 构造 503。
func unavailable(code, message string) *apiErr {
	return &apiErr{class: "unavailable", code: code, message: message, status: http.StatusServiceUnavailable}
}

// tooLarge 构造 413。
func tooLarge(code, message string) *apiErr {
	return &apiErr{class: "too_large", code: code, message: message, status: http.StatusRequestEntityTooLarge}
}
