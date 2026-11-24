package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/btcpay"
)

// writeInvoiceError answers a failed BTCPay invoice create. Every path that
// creates an invoice uses it, so they cannot drift apart.
//
// Transient Lightning trouble is a 503 the caller can retry, not a 500 that
// reads as "your request was wrong". That covers BTCPay saying the Lightning
// node did not reply, and BTCPay itself not answering in time (seen while the
// node was still starting). Any other failure stays a 500.
func writeInvoiceError(c *gin.Context, err error) {
	if errors.Is(err, btcpay.ErrPaymentMethodUnavailable) || errors.Is(err, btcpay.ErrInvoiceTimeout) {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "Lightning is temporarily unavailable, please try again",
			"retries": true,
		})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create invoice"})
}
