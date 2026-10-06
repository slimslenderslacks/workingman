package whatsapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// PhoneNumberInfo is what Graph reports about the configured phone number id.
type PhoneNumberInfo struct {
	ID                 string `json:"id"`
	DisplayPhoneNumber string `json:"display_phone_number"`
	VerifiedName       string `json:"verified_name"`
	QualityRating      string `json:"quality_rating"`
}

// PhoneNumberInfo calls GET /{phone_number_id} to check that the access token
// is valid and the id exists. One attempt, no retries: it backs interactive
// checks (`orch whatsapp setup|status`), where a quick, clear failure beats a
// long backoff. Failures are a *GraphError (the answer was non-2xx) or a
// redacted transport error.
func (c *CloudClient) PhoneNumberInfo(ctx context.Context) (PhoneNumberInfo, error) {
	u := c.cfg.GraphBaseURL + "/" + c.cfg.APIVersion + "/" + c.cfg.PhoneNumberID +
		"?fields=display_phone_number,verified_name,quality_rating"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return PhoneNumberInfo{}, c.wrapTransport(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken.Reveal())
	resp, err := c.http.Do(req)
	if err != nil {
		return PhoneNumberInfo{}, c.wrapTransport(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return PhoneNumberInfo{}, c.wrapTransport(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return PhoneNumberInfo{}, c.graphError(resp, raw)
	}
	var info PhoneNumberInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		// Reachable and authorized, just not in the shape we expected.
		return PhoneNumberInfo{ID: c.cfg.PhoneNumberID}, nil
	}
	for _, s := range []*string{&info.ID, &info.DisplayPhoneNumber, &info.VerifiedName, &info.QualityRating} {
		*s = c.Redact(*s)
	}
	return info, nil
}
