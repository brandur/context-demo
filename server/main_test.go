package main

import (
	"net/http"
	"testing"

	assert "github.com/stretchr/testify/require"
)

func TestAPIError_Error(t *testing.T) {
	err := &APIError{http.StatusBadRequest, "foo", nil}
	assert.Equal(t,
		"API error status 400: foo",
		err.Error(),
	)
}
