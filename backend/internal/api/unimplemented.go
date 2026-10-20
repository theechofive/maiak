package api

// unimplemented lists, explicitly, every operation of the contract that has no handler yet; each
// answers 501 problem+json. It is hand-written on purpose (unlike gen.Unimplemented): a new
// operation in api/openapi.yaml breaks the build until it is added here or implemented on
// server. A task that implements an operation deletes its method here. Since B-15 every
// operation of the MVP contract has a handler, so the type is empty.
type unimplemented struct{}
