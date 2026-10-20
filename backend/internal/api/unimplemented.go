package api

import (
	"context"

	"github.com/theechofive/maiak/backend/internal/api/gen"
)

// unimplemented lists, explicitly, every operation of the contract that has no handler yet; each
// answers 501 problem+json. It is hand-written on purpose (unlike gen.Unimplemented): a new
// operation in api/openapi.yaml breaks the build until it is added here or implemented on
// server. A task that implements an operation deletes its method here.
type unimplemented struct{}

func (unimplemented) ListMonitors(context.Context, gen.ListMonitorsRequestObject) (gen.ListMonitorsResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) CreateMonitor(context.Context, gen.CreateMonitorRequestObject) (gen.CreateMonitorResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) GetMonitor(context.Context, gen.GetMonitorRequestObject) (gen.GetMonitorResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) UpdateMonitor(context.Context, gen.UpdateMonitorRequestObject) (gen.UpdateMonitorResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) DeleteMonitor(context.Context, gen.DeleteMonitorRequestObject) (gen.DeleteMonitorResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) PauseMonitor(context.Context, gen.PauseMonitorRequestObject) (gen.PauseMonitorResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) ResumeMonitor(context.Context, gen.ResumeMonitorRequestObject) (gen.ResumeMonitorResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) GetMonitorStats(context.Context, gen.GetMonitorStatsRequestObject) (gen.GetMonitorStatsResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) ListMonitorChecks(context.Context, gen.ListMonitorChecksRequestObject) (gen.ListMonitorChecksResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) ListMonitorIncidents(context.Context, gen.ListMonitorIncidentsRequestObject) (gen.ListMonitorIncidentsResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) GetSlackSettings(context.Context, gen.GetSlackSettingsRequestObject) (gen.GetSlackSettingsResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) PutSlackSettings(context.Context, gen.PutSlackSettingsRequestObject) (gen.PutSlackSettingsResponseObject, error) {
	return nil, errNotImplemented
}

func (unimplemented) TestSlack(context.Context, gen.TestSlackRequestObject) (gen.TestSlackResponseObject, error) {
	return nil, errNotImplemented
}
