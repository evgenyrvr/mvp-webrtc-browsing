package printing

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	ipp "github.com/OpenPrinting/goipp"
)

const PrinterURI = "ipp://127.0.0.1:8631/printers/local"

// IPP is private to a single application container. It never fetches URLs.
// The queue bounds uploads; pending reserves at most two temporary job IDs.
type IPP struct {
	Queue   *Queue
	Allowed func() bool
	mu      sync.Mutex
	next    int
	pending map[int]time.Time
}

func attr(name string, tag ipp.Tag, value ipp.Value, more ...ipp.Value) ipp.Attribute {
	return ipp.MakeAttr(name, tag, value, more...)
}
func value(m *ipp.Message, name string) ipp.Value {
	for _, group := range []ipp.Attributes{m.Operation, m.Job} {
		for _, a := range group {
			if a.Name == name && len(a.Values) > 0 {
				return a.Values[0].V
			}
		}
	}
	return nil
}
func (s *IPP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "IPP POST required", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxPDF+(64<<10))
	defer r.Body.Close()
	var req ipp.Message
	// Decode consumes exactly the attribute section; bound it independently of PDF.
	if err := req.Decode(io.LimitReader(r.Body, 64<<10)); err != nil {
		http.Error(w, "invalid IPP", 400)
		return
	}
	res := ipp.NewResponse(req.Version, ipp.StatusOk, req.RequestID)
	res.Operation = ipp.Attributes{attr("attributes-charset", ipp.TagCharset, ipp.String("utf-8")), attr("attributes-natural-language", ipp.TagLanguage, ipp.String("en"))}
	fail := func(status ipp.Status) { res.Code = ipp.Code(status) }
	op := ipp.Op(req.Code)
	switch op {
	case ipp.OpGetPrinterAttributes, ipp.OpCupsGetPrinters, ipp.OpCupsGetDefault:
		res.Printer = printerAttributes(s.Allowed())
	case ipp.OpValidateJob, ipp.OpPrintJob, ipp.OpCreateJob, ipp.OpSendDocument:
		if !s.Allowed() {
			fail(ipp.StatusErrorForbidden)
			break
		}
		// libcups may send generated PDF as octet-stream. Queue.Add still
		// validates the PDF signature and size; this does not enable raw jobs.
		if format := value(&req, "document-format"); format != nil && format.String() != "application/pdf" && format.String() != "application/octet-stream" {
			fail(ipp.StatusErrorDocumentFormatNotSupported)
			break
		}
		if op == ipp.OpValidateJob {
			break
		}
		s.mu.Lock()
		if s.pending == nil {
			s.pending = make(map[int]time.Time)
		}
		for id, at := range s.pending {
			if time.Since(at) > time.Minute {
				delete(s.pending, id)
			}
		}
		id := 0
		if op == ipp.OpSendDocument {
			n, ok := value(&req, "job-id").(ipp.Integer)
			id = int(n)
			if _, exists := s.pending[id]; !ok || !exists {
				fail(ipp.StatusErrorNotFound)
				s.mu.Unlock()
				break
			}
			delete(s.pending, id)
			if last, ok := value(&req, "last-document").(ipp.Boolean); !ok || !bool(last) {
				fail(ipp.StatusErrorBadRequest)
				s.mu.Unlock()
				break
			}
		} else {
			if len(s.pending) >= MaxJobs {
				fail(ipp.StatusErrorBusy)
				s.mu.Unlock()
				break
			}
			s.next++
			id = s.next
		}
		if op == ipp.OpCreateJob {
			s.pending[id] = time.Now()
			res.Job = jobAttributes(id, 3)
			s.mu.Unlock()
			break
		}
		s.mu.Unlock()
		_, err := s.Queue.Add(r.Context(), r.Body)
		if err != nil {
			if errors.Is(err, ErrFull) {
				fail(ipp.StatusErrorBusy)
			} else {
				fail(ipp.StatusErrorDocumentFormatError)
			}
		} else if !s.Allowed() {
			s.Queue.Clear()
			fail(ipp.StatusErrorForbidden)
		} else {
			// IPP completion means accepted for delivery, never physical printing.
			res.Job = jobAttributes(id, 9)
		}
	case ipp.OpCancelJob:
		n, ok := value(&req, "job-id").(ipp.Integer)
		s.mu.Lock()
		_, exists := s.pending[int(n)]
		delete(s.pending, int(n))
		s.mu.Unlock()
		if !ok || !exists {
			fail(ipp.StatusErrorNotPossible)
		}
	case ipp.OpGetJobs:
		// Completed PDF deliveries are not exposed as another application's spool.
	default:
		fail(ipp.StatusErrorOperationNotSupported)
	}
	w.Header().Set("Content-Type", "application/ipp")
	slog.Info("IPP request completed", "operation", op.String(), "status", ipp.Status(res.Code).String())
	_ = res.Encode(w)
}
func jobAttributes(id, state int) ipp.Attributes {
	return ipp.Attributes{attr("job-id", ipp.TagInteger, ipp.Integer(id)), attr("job-state", ipp.TagEnum, ipp.Integer(state)), attr("job-state-reasons", ipp.TagKeyword, ipp.String("none"))}
}
func printerAttributes(allowed bool) ipp.Attributes {
	return ipp.Attributes{
		attr("printer-uri-supported", ipp.TagURI, ipp.String(PrinterURI)),
		attr("uri-authentication-supported", ipp.TagKeyword, ipp.String("none")),
		attr("uri-security-supported", ipp.TagKeyword, ipp.String("none")),
		attr("printer-name", ipp.TagName, ipp.String("Local_Printer")),
		attr("printer-info", ipp.TagText, ipp.String("Локальный принтер пользователя")),
		attr("printer-make-and-model", ipp.TagText, ipp.String("MVP1 PDF printer")),
		attr("printer-state", ipp.TagEnum, ipp.Integer(3)),
		attr("printer-state-reasons", ipp.TagKeyword, ipp.String("none")),
		attr("printer-is-accepting-jobs", ipp.TagBoolean, ipp.Boolean(allowed)),
		attr("printer-is-shared", ipp.TagBoolean, ipp.Boolean(false)),
		attr("printer-type", ipp.TagEnum, ipp.Integer(0)),
		attr("queued-job-count", ipp.TagInteger, ipp.Integer(0)),
		attr("ipp-versions-supported", ipp.TagKeyword, ipp.String("1.1"), ipp.String("2.0")),
		attr("operations-supported", ipp.TagEnum, ipp.Integer(ipp.OpPrintJob), ipp.Integer(ipp.OpValidateJob), ipp.Integer(ipp.OpCreateJob), ipp.Integer(ipp.OpSendDocument), ipp.Integer(ipp.OpCancelJob), ipp.Integer(ipp.OpGetJobs), ipp.Integer(ipp.OpGetPrinterAttributes)),
		attr("charset-configured", ipp.TagCharset, ipp.String("utf-8")),
		attr("charset-supported", ipp.TagCharset, ipp.String("utf-8")),
		attr("natural-language-configured", ipp.TagLanguage, ipp.String("en")),
		attr("generated-natural-language-supported", ipp.TagLanguage, ipp.String("en")),
		attr("document-format-default", ipp.TagMimeType, ipp.String("application/pdf")),
		attr("document-format-supported", ipp.TagMimeType, ipp.String("application/pdf"), ipp.String("application/octet-stream")),
		attr("compression-supported", ipp.TagKeyword, ipp.String("none")),
		attr("color-supported", ipp.TagBoolean, ipp.Boolean(true)),
		attr("copies-default", ipp.TagInteger, ipp.Integer(1)),
		attr("copies-supported", ipp.TagRange, ipp.Range{Lower: 1, Upper: 1}),
		attr("media-default", ipp.TagKeyword, ipp.String("iso_a4_210x297mm")),
		attr("media-supported", ipp.TagKeyword, ipp.String("iso_a4_210x297mm"), ipp.String("na_letter_8.5x11in")),
		attr("sides-default", ipp.TagKeyword, ipp.String("one-sided")),
		attr("sides-supported", ipp.TagKeyword, ipp.String("one-sided")),
		attr("multiple-document-jobs-supported", ipp.TagBoolean, ipp.Boolean(false)),
	}
}
