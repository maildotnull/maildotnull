package web

// Provider emulation at the paths the real SDKs call. Every handler returns
// the provider's real success shape so official SDKs treat the send as
// successful, and every message goes through store.Add via ingestRaw.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/maildotnull/maildotnull/internal/faults"
	"github.com/maildotnull/maildotnull/internal/mailutil"
	"github.com/maildotnull/maildotnull/internal/store"
)

// prettyJSON re-encodes a request body the way the TS server stored it
// (JSON.stringify(body, null, 2)) so the UI's "raw" tab looks identical.
func prettyJSON(v any) string {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

func emptyToNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func ensureList(list []string) []string { return emptyToNil(list) }

// --- Resend ---------------------------------------------------------------

// stringOrList accepts JSON that is either a string or an array of strings.
type stringOrList []string

func (s *stringOrList) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*s = mailutil.SplitAddressList(one)
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	out := make([]string, 0, len(many))
	for _, v := range many {
		if v != "" {
			out = append(out, v)
		}
	}
	*s = out
	return nil
}

// Resend — POST https://api.resend.com/emails
// Point the SDK at this server: `new Resend("re_mock", { baseUrl: "http://localhost:8025" })`
func (s *server) handleResend(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"statusCode": 400, "name": "validation_error", "message": "Invalid JSON body",
		})
		return
	}

	var fields struct {
		From        string            `json:"from"`
		To          stringOrList      `json:"to"`
		Cc          stringOrList      `json:"cc"`
		Bcc         stringOrList      `json:"bcc"`
		ReplyTo     stringOrList      `json:"reply_to"`
		ReplyToAlt  stringOrList      `json:"replyTo"`
		Subject     string            `json:"subject"`
		Text        string            `json:"text"`
		HTML        string            `json:"html"`
		Headers     map[string]string `json:"headers"`
		Attachments []struct {
			Filename    string `json:"filename"`
			Content     string `json:"content"`
			ContentType string `json:"content_type"`
		} `json:"attachments"`
	}
	raw, _ := json.Marshal(body)
	_ = json.Unmarshal(raw, &fields)

	if fields.From == "" || len(fields.To) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"statusCode": 422, "name": "missing_required_field", "message": "Missing `from` or `to` field",
		})
		return
	}

	if faults.ApplyHTTPFault(w, r, "resend") {
		return
	}

	attachments := make([]store.Attachment, 0, len(fields.Attachments))
	for _, a := range fields.Attachments {
		content := mailutil.DecodeBase64(a.Content)
		attachments = append(attachments, store.Attachment{
			Filename:    valueOr(a.Filename, "attachment"),
			ContentType: valueOr(a.ContentType, "application/octet-stream"),
			Size:        len(content),
			Content:     content,
		})
	}

	replyTo := append([]string{}, fields.ReplyTo...)
	replyTo = append(replyTo, fields.ReplyToAlt...)

	s.store.Add(store.Email{
		Source:      "resend",
		From:        fields.From,
		To:          fields.To,
		Cc:          ensureList(fields.Cc),
		Bcc:         ensureList(fields.Bcc),
		ReplyTo:     firstOf(replyTo),
		Subject:     fields.Subject,
		Text:        fields.Text,
		HTML:        fields.HTML,
		Headers:     headersOrEmpty(fields.Headers),
		Attachments: attachments,
		Raw:         prettyJSON(body),
	})

	writeJSON(w, http.StatusOK, map[string]string{"id": uuid.NewString()})
}

// --- SendGrid ---------------------------------------------------------------

type sendgridAddress struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (a sendgridAddress) String() string { return mailutil.FormatAddress(a.Email, a.Name) }

func formatSendgrid(list []sendgridAddress) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.String())
	}
	return out
}

// SendGrid — POST https://api.sendgrid.com/v3/mail/send
// Point the SDK at this server: `sgClient.setDefaultRequest("baseUrl", "http://localhost:8025")`
func (s *server) handleSendgrid(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Personalizations []struct {
			To      []sendgridAddress `json:"to"`
			Cc      []sendgridAddress `json:"cc"`
			Bcc     []sendgridAddress `json:"bcc"`
			Subject string            `json:"subject"`
		} `json:"personalizations"`
		From    *sendgridAddress `json:"from"`
		ReplyTo *sendgridAddress `json:"reply_to"`
		Subject string           `json:"subject"`
		Content []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"content"`
		Attachments []struct {
			Content   string `json:"content"`
			Filename  string `json:"filename"`
			Type      string `json:"type"`
			ContentID string `json:"content_id"`
		} `json:"attachments"`
		Headers map[string]string `json:"headers"`
	}
	rawBody := json.RawMessage{}
	if err := readJSON(r, &rawBody); err != nil || json.Unmarshal(rawBody, &body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"errors": []map[string]any{{"message": "Invalid JSON body", "field": nil, "help": nil}},
		})
		return
	}

	if body.From == nil || body.From.Email == "" || len(body.Personalizations) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"errors": []map[string]any{{"message": "Missing from or personalizations", "field": nil, "help": nil}},
		})
		return
	}

	if faults.ApplyHTTPFault(w, r, "sendgrid") {
		return
	}

	var text, html string
	for _, c := range body.Content {
		switch c.Type {
		case "text/plain":
			if text == "" {
				text = c.Value
			}
		case "text/html":
			if html == "" {
				html = c.Value
			}
		}
	}
	attachments := make([]store.Attachment, 0, len(body.Attachments))
	for _, a := range body.Attachments {
		content := mailutil.DecodeBase64(a.Content)
		attachments = append(attachments, store.Attachment{
			Filename:    valueOr(a.Filename, "attachment"),
			ContentType: valueOr(a.Type, "application/octet-stream"),
			Size:        len(content),
			Content:     content,
		})
		// Resolve cid: references to data URIs, as ParseMIME does for SMTP;
		// the sandboxed preview iframe can't fetch cid: parts itself.
		if a.ContentID != "" && html != "" {
			html = strings.ReplaceAll(html, "cid:"+a.ContentID,
				"data:"+valueOr(a.Type, "application/octet-stream")+";base64,"+base64.StdEncoding.EncodeToString(content))
		}
	}
	replyTo := ""
	if body.ReplyTo != nil {
		replyTo = body.ReplyTo.String()
	}

	var pretty any
	_ = json.Unmarshal(rawBody, &pretty)

	// Each personalization is a separate message, like the real API.
	for _, p := range body.Personalizations {
		subject := p.Subject
		if subject == "" {
			subject = body.Subject
		}
		s.store.Add(store.Email{
			Source:      "sendgrid",
			From:        body.From.String(),
			To:          formatSendgrid(p.To),
			Cc:          formatSendgrid(p.Cc),
			Bcc:         formatSendgrid(p.Bcc),
			ReplyTo:     replyTo,
			Subject:     subject,
			Text:        text,
			HTML:        html,
			Headers:     headersOrEmpty(body.Headers),
			Attachments: attachments,
			Raw:         prettyJSON(pretty),
		})
	}

	w.Header().Set("X-Message-Id", uuid.NewString())
	w.WriteHeader(http.StatusAccepted)
}

// --- Mailjet ----------------------------------------------------------------

type mailjetAddress struct {
	Email string `json:"Email"`
	Name  string `json:"Name"`
}

func (a mailjetAddress) String() string { return mailutil.FormatAddress(a.Email, a.Name) }

func formatMailjet(list []mailjetAddress) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.String())
	}
	return out
}

// Mailjet — POST https://api.mailjet.com/v3.1/send
// Point the SDK at this server: `new Mailjet({ apiKey, apiSecret, config: { host: "localhost:8025" } })`
func (s *server) handleMailjet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			From        *mailjetAddress   `json:"From"`
			To          []mailjetAddress  `json:"To"`
			Cc          []mailjetAddress  `json:"Cc"`
			Bcc         []mailjetAddress  `json:"Bcc"`
			ReplyTo     *mailjetAddress   `json:"ReplyTo"`
			Subject     string            `json:"Subject"`
			TextPart    string            `json:"TextPart"`
			HTMLPart    string            `json:"HTMLPart"`
			Headers     map[string]string `json:"Headers"`
			Attachments []struct {
				ContentType   string `json:"ContentType"`
				Filename      string `json:"Filename"`
				Base64Content string `json:"Base64Content"`
			} `json:"Attachments"`
		} `json:"Messages"`
	}
	var rawBody struct {
		Messages []json.RawMessage `json:"Messages"`
	}
	buffered := json.RawMessage{}
	if err := readJSON(r, &buffered); err != nil || json.Unmarshal(buffered, &body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ErrorMessage": "Invalid JSON body", "StatusCode": 400})
		return
	}
	_ = json.Unmarshal(buffered, &rawBody)

	if len(body.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ErrorMessage": "Missing Messages array", "StatusCode": 400})
		return
	}

	if faults.ApplyHTTPFault(w, r, "mailjet") {
		return
	}

	type mailjetRecipient struct {
		Email       string `json:"Email"`
		MessageUUID string `json:"MessageUUID"`
		MessageID   int64  `json:"MessageID"`
		MessageHref string `json:"MessageHref"`
	}
	type mailjetResult struct {
		Status string             `json:"Status"`
		To     []mailjetRecipient `json:"To"`
		Cc     []any              `json:"Cc"`
		Bcc    []any              `json:"Bcc"`
	}

	results := make([]mailjetResult, 0, len(body.Messages))
	for i, m := range body.Messages {
		attachments := make([]store.Attachment, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			content := mailutil.DecodeBase64(a.Base64Content)
			attachments = append(attachments, store.Attachment{
				Filename:    valueOr(a.Filename, "attachment"),
				ContentType: valueOr(a.ContentType, "application/octet-stream"),
				Size:        len(content),
				Content:     content,
			})
		}
		from, replyTo := "", ""
		if m.From != nil {
			from = m.From.String()
		}
		if m.ReplyTo != nil {
			replyTo = m.ReplyTo.String()
		}
		var prettyMsg any
		if i < len(rawBody.Messages) {
			_ = json.Unmarshal(rawBody.Messages[i], &prettyMsg)
		}
		s.store.Add(store.Email{
			Source:      "mailjet",
			From:        from,
			To:          formatMailjet(m.To),
			Cc:          formatMailjet(m.Cc),
			Bcc:         formatMailjet(m.Bcc),
			ReplyTo:     replyTo,
			Subject:     m.Subject,
			Text:        m.TextPart,
			HTML:        m.HTMLPart,
			Headers:     headersOrEmpty(m.Headers),
			Attachments: attachments,
			Raw:         prettyJSON(prettyMsg),
		})

		recipients := make([]mailjetRecipient, 0, len(m.To))
		for _, a := range m.To {
			recipients = append(recipients, mailjetRecipient{
				Email:       a.Email,
				MessageUUID: uuid.NewString(),
				MessageID:   rand.Int64N(1e15),
				MessageHref: "http://localhost:8025",
			})
		}
		results = append(results, mailjetResult{Status: "success", To: recipients, Cc: []any{}, Bcc: []any{}})
	}

	writeJSON(w, http.StatusOK, map[string]any{"Messages": results})
}

// --- Mailgun ----------------------------------------------------------------

// Mailgun — POST https://api.mailgun.net/v3/{domain}/messages (multipart or urlencoded)
// Point the SDK at this server: `mailgun.client({ username: "api", key: "mock", url: "http://localhost:8025" })`
func (s *server) handleMailgun(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")

	contentType := r.Header.Get("Content-Type")
	isMultipart := strings.HasPrefix(contentType, "multipart/form-data")
	var parseErr error
	if isMultipart {
		parseErr = r.ParseMultipartForm(64 << 20)
	} else {
		parseErr = r.ParseForm()
	}
	if parseErr != nil {
		writeJSON(w, http.StatusBadRequest, errBody{"Invalid form body"})
		return
	}

	form := r.Form
	if isMultipart && r.MultipartForm != nil {
		form = r.MultipartForm.Value
	}

	getAll := func(name string) []string {
		var out []string
		for _, v := range form[name] {
			out = append(out, mailutil.SplitAddressList(v)...)
		}
		return out
	}
	get := func(name string) string {
		if vs := form[name]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}

	from := get("from")
	to := getAll("to")
	if from == "" || len(to) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody{"'from' or 'to' parameter is missing"})
		return
	}

	if faults.ApplyHTTPFault(w, r, "mailgun") {
		return
	}

	var attachments []store.Attachment
	var fileLines []string
	if r.MultipartForm != nil {
		for _, field := range []string{"attachment", "inline"} {
			for _, fh := range r.MultipartForm.File[field] {
				f, err := fh.Open()
				if err != nil {
					continue
				}
				content := make([]byte, fh.Size)
				n, _ := f.Read(content)
				f.Close()
				name := valueOr(fh.Filename, "attachment")
				attachments = append(attachments, store.Attachment{
					Filename:    name,
					ContentType: valueOr(fh.Header.Get("Content-Type"), "application/octet-stream"),
					Size:        int(fh.Size),
					Content:     content[:n],
				})
				fileLines = append(fileLines, fmt.Sprintf("%s: <file %s, %d bytes>", field, name, fh.Size))
			}
		}
	}

	headers := map[string]string{}
	for key, values := range form {
		if strings.HasPrefix(key, "h:") && len(values) > 0 {
			headers[key[2:]] = values[0]
		}
	}

	var rawLines []string
	for key, values := range form {
		for _, v := range values {
			rawLines = append(rawLines, key+": "+v)
		}
	}
	rawLines = append(rawLines, fileLines...)

	s.store.Add(store.Email{
		Source:      "mailgun",
		From:        from,
		To:          to,
		Cc:          emptyToNil(getAll("cc")),
		Bcc:         emptyToNil(getAll("bcc")),
		ReplyTo:     headers["Reply-To"],
		Subject:     get("subject"),
		Text:        get("text"),
		HTML:        get("html"),
		Headers:     headers,
		Attachments: attachments,
		Raw:         strings.Join(rawLines, "\n"),
	})

	writeJSON(w, http.StatusOK, map[string]string{
		"id":      "<" + uuid.NewString() + "@" + domain + ">",
		"message": "Queued. Thank you.",
	})
}

// --- Postmark ---------------------------------------------------------------

type postmarkMessage struct {
	From     string `json:"From"`
	To       string `json:"To"`
	Cc       string `json:"Cc"`
	Bcc      string `json:"Bcc"`
	ReplyTo  string `json:"ReplyTo"`
	Subject  string `json:"Subject"`
	TextBody string `json:"TextBody"`
	HTMLBody string `json:"HtmlBody"`
	Headers  []struct {
		Name  string `json:"Name"`
		Value string `json:"Value"`
	} `json:"Headers"`
	Attachments []struct {
		Name        string `json:"Name"`
		Content     string `json:"Content"`
		ContentType string `json:"ContentType"`
	} `json:"Attachments"`
}

type postmarkResult struct {
	To          string `json:"To,omitempty"`
	SubmittedAt string `json:"SubmittedAt,omitempty"`
	MessageID   string `json:"MessageID,omitempty"`
	ErrorCode   int    `json:"ErrorCode"`
	Message     string `json:"Message"`
}

func (s *server) ingestPostmarkMessage(m postmarkMessage, raw json.RawMessage) postmarkResult {
	if m.From == "" || m.To == "" {
		return postmarkResult{ErrorCode: 300, Message: "Invalid 'From' or 'To' field."}
	}
	headers := map[string]string{}
	for _, h := range m.Headers {
		headers[h.Name] = h.Value
	}
	attachments := make([]store.Attachment, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		content := mailutil.DecodeBase64(a.Content)
		attachments = append(attachments, store.Attachment{
			Filename:    valueOr(a.Name, "attachment"),
			ContentType: valueOr(a.ContentType, "application/octet-stream"),
			Size:        len(content),
			Content:     content,
		})
	}
	var pretty any
	_ = json.Unmarshal(raw, &pretty)
	s.store.Add(store.Email{
		Source:      "postmark",
		From:        m.From,
		To:          mailutil.SplitAddressList(m.To),
		Cc:          emptyToNil(mailutil.SplitAddressList(m.Cc)),
		Bcc:         emptyToNil(mailutil.SplitAddressList(m.Bcc)),
		ReplyTo:     m.ReplyTo,
		Subject:     m.Subject,
		Text:        m.TextBody,
		HTML:        m.HTMLBody,
		Headers:     headers,
		Attachments: attachments,
		Raw:         prettyJSON(pretty),
	})
	return postmarkResult{
		To:          m.To,
		SubmittedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		MessageID:   uuid.NewString(),
		ErrorCode:   0,
		Message:     "OK",
	}
}

// Postmark — POST https://api.postmarkapp.com/email
// Point the SDK at this server: `new postmark.ServerClient("mock", { requestHost: "localhost:8025", useHttps: false })`
func (s *server) handlePostmark(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	var m postmarkMessage
	if err := readJSON(r, &raw); err != nil || json.Unmarshal(raw, &m) != nil {
		writeJSON(w, http.StatusUnprocessableEntity, postmarkResult{ErrorCode: 300, Message: "Invalid JSON body"})
		return
	}
	if faults.ApplyHTTPFault(w, r, "postmark") {
		return
	}
	result := s.ingestPostmarkMessage(m, raw)
	status := http.StatusOK
	if result.ErrorCode != 0 {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, result)
}

// Postmark batch — POST https://api.postmarkapp.com/email/batch
func (s *server) handlePostmarkBatch(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := readJSON(r, &raw); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, postmarkResult{ErrorCode: 300, Message: "Invalid JSON body"})
		return
	}
	var rawList []json.RawMessage
	if json.Unmarshal(raw, &rawList) != nil {
		writeJSON(w, http.StatusUnprocessableEntity, postmarkResult{ErrorCode: 300, Message: "Expected an array"})
		return
	}
	if faults.ApplyHTTPFault(w, r, "postmark") {
		return
	}
	results := make([]postmarkResult, 0, len(rawList))
	for _, itemRaw := range rawList {
		var m postmarkMessage
		_ = json.Unmarshal(itemRaw, &m)
		results = append(results, s.ingestPostmarkMessage(m, itemRaw))
	}
	writeJSON(w, http.StatusOK, results)
}

// --- Brevo ------------------------------------------------------------------

type brevoAddress struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (a brevoAddress) String() string { return mailutil.FormatAddress(a.Email, a.Name) }

func formatBrevo(list []brevoAddress) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.String())
	}
	return out
}

// Brevo (Sendinblue) — POST https://api.brevo.com/v3/smtp/email
// Point the SDK at this server by overriding its base path to `http://localhost:8025/v3`.
func (s *server) handleBrevo(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	var body struct {
		Sender      *brevoAddress     `json:"sender"`
		To          []brevoAddress    `json:"to"`
		Cc          []brevoAddress    `json:"cc"`
		Bcc         []brevoAddress    `json:"bcc"`
		ReplyTo     *brevoAddress     `json:"replyTo"`
		Subject     string            `json:"subject"`
		HTMLContent string            `json:"htmlContent"`
		TextContent string            `json:"textContent"`
		Headers     map[string]string `json:"headers"`
		Attachment  []struct {
			Content string `json:"content"`
			Name    string `json:"name"`
			URL     string `json:"url"`
		} `json:"attachment"`
	}
	if err := readJSON(r, &raw); err != nil || json.Unmarshal(raw, &body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_parameter", "message": "Invalid JSON body"})
		return
	}

	if body.Sender == nil || body.Sender.Email == "" || len(body.To) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "missing_parameter", "message": "sender and to are required"})
		return
	}

	if faults.ApplyHTTPFault(w, r, "brevo") {
		return
	}

	var attachments []store.Attachment
	for _, a := range body.Attachment {
		if a.Content == "" {
			continue
		}
		content := mailutil.DecodeBase64(a.Content)
		attachments = append(attachments, store.Attachment{
			Filename:    valueOr(a.Name, "attachment"),
			ContentType: "application/octet-stream",
			Size:        len(content),
			Content:     content,
		})
	}
	replyTo := ""
	if body.ReplyTo != nil {
		replyTo = body.ReplyTo.String()
	}
	var pretty any
	_ = json.Unmarshal(raw, &pretty)

	s.store.Add(store.Email{
		Source:      "brevo",
		From:        body.Sender.String(),
		To:          formatBrevo(body.To),
		Cc:          formatBrevo(body.Cc),
		Bcc:         formatBrevo(body.Bcc),
		ReplyTo:     replyTo,
		Subject:     body.Subject,
		Text:        body.TextContent,
		HTML:        body.HTMLContent,
		Headers:     headersOrEmpty(body.Headers),
		Attachments: attachments,
		Raw:         prettyJSON(pretty),
	})

	writeJSON(w, http.StatusCreated, map[string]string{"messageId": "<" + uuid.NewString() + "@maildotnull>"})
}

// --- Amazon SES v2 ----------------------------------------------------------

// Amazon SES v2 — POST /v2/email/outbound-emails
// Point the SDK at this server: `new SESv2Client({ endpoint: "http://localhost:8025", region: "eu-west-1", credentials: { accessKeyId: "mock", secretAccessKey: "mock" } })`
func (s *server) handleSes(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	var body struct {
		FromEmailAddress string `json:"FromEmailAddress"`
		Destination      *struct {
			ToAddresses  []string `json:"ToAddresses"`
			CcAddresses  []string `json:"CcAddresses"`
			BccAddresses []string `json:"BccAddresses"`
		} `json:"Destination"`
		ReplyToAddresses []string `json:"ReplyToAddresses"`
		Content          *struct {
			Simple *struct {
				Subject *struct {
					Data string `json:"Data"`
				} `json:"Subject"`
				Body *struct {
					Text *struct {
						Data string `json:"Data"`
					} `json:"Text"`
					HTML *struct {
						Data string `json:"Data"`
					} `json:"Html"`
				} `json:"Body"`
			} `json:"Simple"`
			Raw *struct {
				Data string `json:"Data"`
			} `json:"Raw"`
		} `json:"Content"`
	}
	if err := readJSON(r, &raw); err != nil || json.Unmarshal(raw, &body) != nil {
		writeJSON(w, http.StatusBadRequest, errBody{"Invalid JSON body"})
		return
	}

	if faults.ApplyHTTPFault(w, r, "ses") {
		return
	}

	if body.Content != nil && body.Content.Raw != nil && body.Content.Raw.Data != "" {
		// Raw MIME message, base64-encoded — parse it like an SMTP delivery.
		mime, err := base64.StdEncoding.DecodeString(body.Content.Raw.Data)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody{"Invalid base64 in Content.Raw.Data"})
			return
		}
		parsed, err := mailutil.ParseMIME(mime)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody{"Invalid MIME message in Content.Raw.Data"})
			return
		}
		from := body.FromEmailAddress
		if from == "" {
			from = parsed.From
		}
		to := parsed.To
		cc := []string{}
		bcc := []string{}
		if body.Destination != nil {
			if len(body.Destination.ToAddresses) > 0 {
				to = body.Destination.ToAddresses
			}
			cc = emptyToNil(body.Destination.CcAddresses)
			bcc = emptyToNil(body.Destination.BccAddresses)
		}
		s.store.Add(store.Email{
			Source:      "ses",
			From:        from,
			To:          emptyToNil(to),
			Cc:          cc,
			Bcc:         bcc,
			Subject:     parsed.Subject,
			Text:        parsed.Text,
			HTML:        parsed.HTML,
			Headers:     parsed.Headers,
			Attachments: parsed.Attachments,
			Raw:         string(mime),
		})
		writeJSON(w, http.StatusOK, map[string]string{"MessageId": uuid.NewString()})
		return
	}

	if body.FromEmailAddress == "" || body.Destination == nil || len(body.Destination.ToAddresses) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody{"FromEmailAddress and Destination.ToAddresses are required"})
		return
	}

	subject, text, html := "", "", ""
	if body.Content != nil && body.Content.Simple != nil {
		if body.Content.Simple.Subject != nil {
			subject = body.Content.Simple.Subject.Data
		}
		if body.Content.Simple.Body != nil {
			if body.Content.Simple.Body.Text != nil {
				text = body.Content.Simple.Body.Text.Data
			}
			if body.Content.Simple.Body.HTML != nil {
				html = body.Content.Simple.Body.HTML.Data
			}
		}
	}
	var pretty any
	_ = json.Unmarshal(raw, &pretty)

	s.store.Add(store.Email{
		Source:      "ses",
		From:        body.FromEmailAddress,
		To:          body.Destination.ToAddresses,
		Cc:          emptyToNil(body.Destination.CcAddresses),
		Bcc:         emptyToNil(body.Destination.BccAddresses),
		ReplyTo:     firstOf(body.ReplyToAddresses),
		Subject:     subject,
		Text:        text,
		HTML:        html,
		Headers:     map[string]string{},
		Attachments: []store.Attachment{},
		Raw:         prettyJSON(pretty),
	})

	writeJSON(w, http.StatusOK, map[string]string{"MessageId": uuid.NewString()})
}

// --- small shared helpers ---------------------------------------------------

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func firstOf(list []string) string {
	if len(list) > 0 {
		return list[0]
	}
	return ""
}

func headersOrEmpty(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	return h
}
