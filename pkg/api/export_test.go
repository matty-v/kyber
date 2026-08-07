package api

// MachineToResponseForTest exposes the unexported machineToResponse function
// to the external api_test package. This file is only compiled during tests.
var MachineToResponseForTest = machineToResponse

// ExecStreamParamsForTest exposes the unexported execStreamParams function
// to api_test. Used by the kyber#264 regression test that pins the
// container query-param so the multi-container post-#248 pod doesn't
// regress again.
var ExecStreamParamsForTest = execStreamParams
