package subscription

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"xraytool/internal/domain"
	"xraytool/internal/mocks"
)

func TestApplyBatchOperationsStopsWhenUpdatePreparationFails(t *testing.T) {
	engine := new(mocks.Engine)
	user := domain.VPNUserConfig{Email: "user@example.test"}
	engine.On("RemoveUsersBulk", mock.Anything, []string{user.Email}).Return(errors.New("xray unavailable")).Once()

	result := ApplyBatchOperations(engine, domain.BatchPayload{Add: []domain.VPNUserConfig{user}})

	require.False(t, result.Ok)
	require.Contains(t, result.Error, "preparing user updates")
	engine.AssertNotCalled(t, "AddUsersBulk", mock.Anything, mock.Anything)
	engine.AssertExpectations(t)
}

func TestApplyBatchOperationsStopsWhenExplicitRemovalFails(t *testing.T) {
	engine := new(mocks.Engine)
	engine.On("RemoveUsersBulk", mock.Anything, []string{"remove@example.test"}).Return(errors.New("xray unavailable")).Once()

	result := ApplyBatchOperations(engine, domain.BatchPayload{
		Remove: []string{"remove@example.test"},
		Add:    []domain.VPNUserConfig{{Email: "add@example.test"}},
	})

	require.False(t, result.Ok)
	require.Contains(t, result.Error, "removing users")
	engine.AssertNotCalled(t, "AddUsersBulk", mock.Anything, mock.Anything)
	engine.AssertExpectations(t)
}
