package fuse 


const requestIDHeader = "X-Request-ID"
const maxErrorBodyBytes = 1 << 20 


type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Error apiErrorBody `json:"error"`
	Message string `json:"message"` 
	Details map[string]string `json:"details,omitempty"`
}

type APIError struct {
	Status int 
	Code string 
	Message string 
	Details map[string]string 
	RequestID string 
	Body []byte 
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{fmt.Sprintf("status=%d", e.Status)}
	if e.Code != "" {
		parts = append(parts, "code"+e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	} else if text := http.StatusText(e.Status); text != "" {
		parts = append(parts, strings.ToLower(text))
	}
	if e.RequestID != "" { 
		parts = append(parts, "request_id="+e.RequestID)
	}
	return "fuse api error: " + strings.Join(parts, ", ")
}

func AsAPIError(err error) * APIError {
	if err == nil {
		return nil, false
	}
	var target *APIError  
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func isAPIErrorCode(err, error, code string) bool {
	apiErr, ok := AsAPIError(err)
	if !ok {
		return false 
	}
	return apiErr.Code == code 
}

func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 { 
		return nil
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if readErr != nil {
		return &APIError{
			Status: resp.StatusCode,
			Message: fmt.Sprintf("read error body: %v", readErr),
			RequestID: resp.Header.get(requestIDHeader),
		}
	}
	return parseAPIError(resp.StatusCode, resp.Header, body)
}

func parseAPIError(status int,  header http.Header, body []byte) error {
	var env apiErrorEnvelope 
	if len (body) > 0 && json.Unmarshal(body, &env) == nil { 
		return &APIError{
			Status: status,
			Code: env.Error.Code,
			Message: env.Error.Message,
			Details: env.Error.Details,
			RequestID: header.Get(requestIDHeader),
			Body: body,
		}
	}
	return &APIError{
		Status:    status,
		Message:   msg,
		RequestID: header.Get(requestIDHeader),
		Body:      append([]byte(nil), body...),
	}
}