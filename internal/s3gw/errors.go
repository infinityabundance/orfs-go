package s3gw

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"

	"github.com/infinityabundance/orfs-go/internal/orfserr"
)

// S3 error codes this gateway can return. Anything not listed here is a bug in
// the caller, not a protocol answer.
const (
	codeNoSuchBucket                 = "NoSuchBucket"
	codeNoSuchKey                    = "NoSuchKey"
	codeAccessDenied                 = "AccessDenied"
	codeSignatureDoesNotMatch        = "SignatureDoesNotMatch"
	codeInvalidAccessKeyID           = "InvalidAccessKeyId"
	codeRequestTimeTooSkewed         = "RequestTimeTooSkewed"
	codeAuthorizationHeaderMalformed = "AuthorizationHeaderMalformed"
	codeInvalidArgument              = "InvalidArgument"
	codeInvalidBucketName            = "InvalidBucketName"
	codeInvalidRequest               = "InvalidRequest"
	codeMissingContentLength         = "MissingContentLength"
	codeEntityTooLarge               = "EntityTooLarge"
	codeBadDigest                    = "BadDigest"
	codeContentSHA256Mismatch        = "XAmzContentSHA256Mismatch"
	codeMethodNotAllowed             = "MethodNotAllowed"
	codeNotImplemented               = "NotImplemented"
	codeInternalError                = "InternalError"
	codeBucketNotEmpty               = "BucketNotEmpty"
	codeServiceUnavailable           = "ServiceUnavailable"
)

// httpStatusForCode is the S3 HTTP status for each code.
var httpStatusForCode = map[string]int{
	codeNoSuchBucket:                 http.StatusNotFound,
	codeNoSuchKey:                    http.StatusNotFound,
	codeAccessDenied:                 http.StatusForbidden,
	codeSignatureDoesNotMatch:        http.StatusForbidden,
	codeInvalidAccessKeyID:           http.StatusForbidden,
	codeRequestTimeTooSkewed:         http.StatusForbidden,
	codeAuthorizationHeaderMalformed: http.StatusBadRequest,
	codeInvalidArgument:              http.StatusBadRequest,
	codeInvalidBucketName:            http.StatusBadRequest,
	codeInvalidRequest:               http.StatusBadRequest,
	codeMissingContentLength:         http.StatusLengthRequired,
	codeEntityTooLarge:               http.StatusBadRequest,
	codeBadDigest:                    http.StatusBadRequest,
	codeContentSHA256Mismatch:        http.StatusBadRequest,
	codeMethodNotAllowed:             http.StatusMethodNotAllowed,
	codeNotImplemented:               http.StatusNotImplemented,
	codeInternalError:                http.StatusInternalServerError,
	codeBucketNotEmpty:               http.StatusConflict,
	codeServiceUnavailable:           http.StatusServiceUnavailable,
}

// errorDocument is the <Error> response body.
type errorDocument struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
}

// apiError is a protocol refusal carrying an S3 error code. Handlers return
// one (or an OrFS error) and the request dispatcher renders it.
type apiError struct {
	code    string
	message string
}

func (e *apiError) Error() string { return e.code + ": " + e.message }

func apiErrf(code, format string, a ...any) *apiError {
	return &apiError{code: code, message: fmt.Sprintf(format, a...)}
}

// s3ErrorOf maps any error to an S3 error code and message. An OrFS typed
// refusal is mapped to the honest protocol answer (a capacity refusal is a
// 503, not a silent success); anything unrecognised is an InternalError.
func s3ErrorOf(err error) (code, message string) {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.code, ae.message
	}
	var oe *orfserr.Error
	if errors.As(err, &oe) {
		switch oe.Kind {
		case orfserr.KindNotFound:
			return codeNoSuchKey, oe.Msg
		case orfserr.KindNotDir:
			return codeNoSuchKey, oe.Msg
		case orfserr.KindIsDir:
			return codeInvalidRequest, oe.Msg
		case orfserr.KindNameTooLong, orfserr.KindInvalid:
			return codeInvalidArgument, oe.Msg
		case orfserr.KindNoSpace, orfserr.KindCapacityExhausted:
			return codeServiceUnavailable, "the OrFS volume refused growth: " + oe.Msg
		case orfserr.KindReadOnly:
			return codeServiceUnavailable, "the OrFS volume is read-only: " + oe.Msg
		}
	}
	return codeInternalError, err.Error()
}

// writeS3Error renders an S3 error document with the code's HTTP status.
func writeS3Error(w http.ResponseWriter, r *http.Request, requestID, code, message string) {
	status, ok := httpStatusForCode[code]
	if !ok {
		status = http.StatusInternalServerError
	}
	writeXML(w, status, errorDocument{
		Code:      code,
		Message:   message,
		Resource:  r.URL.Path,
		RequestID: requestID,
	})
}

// writeXML renders v as XML with the given status.
func writeXML(w http.ResponseWriter, status int, v any) {
	body, err := xml.Marshal(v)
	if err != nil {
		http.Error(w, "xml marshalling failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	w.Write(body)
}
