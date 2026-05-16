// api_portal_get_page.go — POST /api/v1/portal/get-page handler
// (sty_02ad5eb9). Mirrors mcpserver.handlePortalGetPage: a thin wire
// adapter that decodes the JSON body, hands the typed input to
// *client.Client.PortalGetPage, and writes the `{status, body}` JSON
// envelope.
//
// pr_mcp_cli_shared_path: every business-logic concern (auth, path
// validation, loopback request, redirect-not-followed) lives in
// internal/client. This handler does not import any substrate package
// directly.

package httpserver

import (
	"net/http"
	"strings"

	"github.com/bobmcallan/satellites/internal/client"
)

func (a *APIRegistrar) handlePortalGetPage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	cc := a.clientCaller(r)
	out, err := a.client.PortalGetPage(r.Context(), cc, client.PortalGetPageInput{Path: req.Path})
	if err != nil {
		// Path validation errors (AC2) are caller-input failures and
		// must surface as 400. Default writeAPIError's keyword scan
		// would treat "prefix /api/ blocked" as a 500.
		msg := err.Error()
		if strings.HasPrefix(msg, "path ") || strings.HasPrefix(msg, "prefix ") {
			writeAPIStatus(w, http.StatusBadRequest, msg)
			return
		}
		writeAPIError(w, err)
		return
	}
	writeAPIJSON(w, out)
}
