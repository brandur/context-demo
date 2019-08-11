package main

import (
	"fmt"
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

func TestAPIError_WithInternalError(t *testing.T) {
	apiErr := &APIError{http.StatusBadRequest, "foo", nil}
	internalErr := fmt.Errorf("internal error")

	dupErr := apiErr.WithInternalError(internalErr)

	// New error should be a duplicate with an internal error.
	assert.Equal(t, apiErr.StatusCode, dupErr.StatusCode)
	assert.Equal(t, apiErr.Message, dupErr.Message)
	assert.Equal(t, internalErr, dupErr.internalErr)

	// The original error should still have a `nil` internal error.
	assert.Nil(t, apiErr.internalErr)
}
