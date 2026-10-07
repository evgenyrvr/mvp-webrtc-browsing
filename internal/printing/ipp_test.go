package printing

import (
	"bytes"
	"net/http/httptest"
	"testing"

	ipp "github.com/OpenPrinting/goipp"
)

func TestIPPDiscoveryUploadAndPermission(t *testing.T) {
	q := &Queue{}
	allowed := true
	s := &IPP{Queue: q, Allowed: func() bool { return allowed }}
	call := func(op ipp.Op, attrs ipp.Attributes, document []byte) *ipp.Message {
		t.Helper()
		req := ipp.NewRequest(ipp.DefaultVersion, op, 7)
		req.Operation = attrs
		var body bytes.Buffer
		if err := req.Encode(&body); err != nil {
			t.Fatal(err)
		}
		body.Write(document)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", "/printers/local", &body))
		var res ipp.Message
		if err := res.Decode(w.Body); err != nil {
			t.Fatal(err, w.Body.String())
		}
		return &res
	}
	res := call(ipp.OpCupsGetPrinters, nil, nil)
	if res.Code != 0 || len(res.Printer) == 0 {
		t.Fatal(res)
	}
	res = call(ipp.OpCreateJob, nil, nil)
	id := value(res, "job-id")
	if id == nil {
		t.Fatal(res)
	}
	attrs := ipp.Attributes{attr("job-id", ipp.TagInteger, id), attr("last-document", ipp.TagBoolean, ipp.Boolean(true)), attr("document-format", ipp.TagMimeType, ipp.String("application/pdf"))}
	res = call(ipp.OpSendDocument, attrs, pdf())
	if res.Code != 0 || q.Head() == nil {
		t.Fatal(res)
	}
	if !bytes.Equal(q.Head().Data, pdf()) {
		t.Fatal("PDF changed by IPP decoder")
	}
	res = call(ipp.OpSendDocument, attrs, pdf())
	if res.Code != ipp.Code(ipp.StatusErrorNotFound) {
		t.Fatal("duplicate job accepted")
	}
	allowed = false
	res = call(ipp.OpPrintJob, nil, pdf())
	if res.Code != ipp.Code(ipp.StatusErrorForbidden) {
		t.Fatal("permission not checked")
	}
	allowed = true
	q.Clear()
	opaque := ipp.Attributes{attr("document-format", ipp.TagMimeType, ipp.String("application/octet-stream"))}
	res = call(ipp.OpPrintJob, opaque, []byte("not a PDF"))
	if res.Code != ipp.Code(ipp.StatusErrorDocumentFormatError) || q.Head() != nil {
		t.Fatal("octet-stream bypassed PDF validation")
	}
	res = call(ipp.OpPrintJob, opaque, pdf())
	if res.Code != 0 || q.Head() == nil || !bytes.Equal(q.Head().Data, pdf()) {
		t.Fatal("libcups PDF octet-stream not accepted")
	}
	res = call(ipp.OpPrintURI, nil, nil)
	if res.Code != ipp.Code(ipp.StatusErrorOperationNotSupported) {
		t.Fatal("URL submission supported")
	}
}
