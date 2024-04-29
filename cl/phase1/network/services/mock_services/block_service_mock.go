// Code generated by MockGen. DO NOT EDIT.
// Source: github.com/ledgerwatch/erigon/cl/phase1/network/services (interfaces: BlockService)
//
// Generated by this command:
//
//	mockgen -typed=true -destination=./mock_services/block_service_mock.go -package=mock_services . BlockService
//

// Package mock_services is a generated GoMock package.
package mock_services

import (
	context "context"
	reflect "reflect"

	cltypes "github.com/ledgerwatch/erigon/cl/cltypes"
	gomock "go.uber.org/mock/gomock"
)

// MockBlockService is a mock of BlockService interface.
type MockBlockService struct {
	ctrl     *gomock.Controller
	recorder *MockBlockServiceMockRecorder
}

// MockBlockServiceMockRecorder is the mock recorder for MockBlockService.
type MockBlockServiceMockRecorder struct {
	mock *MockBlockService
}

// NewMockBlockService creates a new mock instance.
func NewMockBlockService(ctrl *gomock.Controller) *MockBlockService {
	mock := &MockBlockService{ctrl: ctrl}
	mock.recorder = &MockBlockServiceMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockBlockService) EXPECT() *MockBlockServiceMockRecorder {
	return m.recorder
}

// ProcessMessage mocks base method.
func (m *MockBlockService) ProcessMessage(arg0 context.Context, arg1 *uint64, arg2 *cltypes.SignedBeaconBlock) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "ProcessMessage", arg0, arg1, arg2)
	ret0, _ := ret[0].(error)
	return ret0
}

// ProcessMessage indicates an expected call of ProcessMessage.
func (mr *MockBlockServiceMockRecorder) ProcessMessage(arg0, arg1, arg2 any) *MockBlockServiceProcessMessageCall {
	mr.mock.ctrl.T.Helper()
	call := mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ProcessMessage", reflect.TypeOf((*MockBlockService)(nil).ProcessMessage), arg0, arg1, arg2)
	return &MockBlockServiceProcessMessageCall{Call: call}
}

// MockBlockServiceProcessMessageCall wrap *gomock.Call
type MockBlockServiceProcessMessageCall struct {
	*gomock.Call
}

// Return rewrite *gomock.Call.Return
func (c *MockBlockServiceProcessMessageCall) Return(arg0 error) *MockBlockServiceProcessMessageCall {
	c.Call = c.Call.Return(arg0)
	return c
}

// Do rewrite *gomock.Call.Do
func (c *MockBlockServiceProcessMessageCall) Do(f func(context.Context, *uint64, *cltypes.SignedBeaconBlock) error) *MockBlockServiceProcessMessageCall {
	c.Call = c.Call.Do(f)
	return c
}

// DoAndReturn rewrite *gomock.Call.DoAndReturn
func (c *MockBlockServiceProcessMessageCall) DoAndReturn(f func(context.Context, *uint64, *cltypes.SignedBeaconBlock) error) *MockBlockServiceProcessMessageCall {
	c.Call = c.Call.DoAndReturn(f)
	return c
}
