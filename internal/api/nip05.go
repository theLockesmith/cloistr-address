package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/metrics"
)

// NIP05Response represents the nostr.json response format
type NIP05Response struct {
	Names  map[string]string   `json:"names"`
	Relays map[string][]string `json:"relays,omitempty"`
}

// handleNIP05 handles NIP-05 verification requests
// GET /.well-known/nostr.json?name=username
func (h *Handler) handleNIP05(c *gin.Context) {
	// Get optional name parameter
	name := c.Query("name")

	ctx := c.Request.Context()

	// If name is specified, look up single user
	if name != "" {
		addr, err := h.store.GetAddressByUsername(ctx, name, h.cfg.Domain)
		if err != nil {
			slog.Error("failed to get address", "username", name, "error", err)
			metrics.NIP05Requests.WithLabelValues("error").Inc()
			c.JSON(http.StatusOK, NIP05Response{
				Names: map[string]string{},
			})
			return
		}

		if addr == nil {
			metrics.NIP05Requests.WithLabelValues("not_found").Inc()
			c.JSON(http.StatusOK, NIP05Response{
				Names: map[string]string{},
			})
			return
		}

		// Get relays for this address
		relays, err := h.store.GetRelaysForAddress(ctx, addr.ID)
		if err != nil {
			slog.Warn("failed to get relays for address", "username", name, "error", err)
			// Continue without relays
		}

		// If no relays configured, use defaults
		if len(relays) == 0 {
			relays = h.cfg.Relays
		}

		response := NIP05Response{
			Names: map[string]string{
				addr.Username: addr.Pubkey,
			},
		}

		if len(relays) > 0 {
			response.Relays = map[string][]string{
				addr.Pubkey: relays,
			}
		}

		metrics.NIP05Requests.WithLabelValues("found").Inc()
		c.JSON(http.StatusOK, response)
		return
	}

	// No name specified - return all addresses (optional bulk behavior)
	// Some implementations return empty, others return all users
	// We return empty for privacy and performance
	metrics.NIP05Requests.WithLabelValues("found").Inc()
	c.JSON(http.StatusOK, NIP05Response{
		Names: map[string]string{},
	})
}
