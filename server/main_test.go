package main

import (
	"encoding/json"
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

func TestAPIError_MarshalJSON(t *testing.T) {
	// Case with internal error
	{
		apiErr := &APIError{
			http.StatusBadRequest,
			"message",
			fmt.Errorf("internal error"),
		}

		data, err := json.Marshal(apiErr)
		assert.NoError(t, err)

		var actualErr APIError
		err = json.Unmarshal(data, &actualErr)
		assert.NoError(t, err)

		assert.Equal(t, "message (internal error)", actualErr.Message)
	}

	// Case of no internal error
	{
		apiErr := &APIError{http.StatusBadRequest, "message", nil}

		data, err := json.Marshal(apiErr)
		assert.NoError(t, err)

		var actualErr APIError
		err = json.Unmarshal(data, &actualErr)
		assert.NoError(t, err)

		assert.Equal(t, "message", actualErr.Message)
	}

	// Case of 500. These are not expected and therefore anything could be in
	// the internal error. So as not to expose anything, we don't add any
	// context in this case.
	{
		apiErr := &APIError{
			http.StatusInternalServerError,
			"message",
			fmt.Errorf("internal error"),
		}

		data, err := json.Marshal(apiErr)
		assert.NoError(t, err)

		var actualErr APIError
		err = json.Unmarshal(data, &actualErr)
		assert.NoError(t, err)

		assert.Equal(t, "message", actualErr.Message)
	}
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
