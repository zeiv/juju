// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package apiserver_test

import (
	. "launchpad.net/gocheck"
	"launchpad.net/juju-core/juju/testing"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api"
	"launchpad.net/juju-core/state/api/params"
	"launchpad.net/juju-core/state/apiserver"
)

type machinerSuite struct {
	testing.JujuConnSuite

	machiner *apiserver.SrvMachiner

	machine0 *state.Machine
	machine1 *state.Machine
}

var _ = Suite(&machinerSuite{})

// fakeAuthorizer implements the Authorizer interface.
type fakeAuthorizer struct {
	tag     string
	manager bool
}

func (fa *fakeAuthorizer) AuthOwner(entity apiserver.Tagger) bool {
	return entity.Tag() == fa.tag
}

func (fa *fakeAuthorizer) AuthEnvironManager() bool {
	return fa.manager
}

func (s *machinerSuite) SetUpTest(c *C) {
	s.JujuConnSuite.SetUpTest(c)

	// Create a machine so that we can login as its agent
	var err error
	s.machine0, err = s.State.AddMachine("series", state.JobManageEnviron)
	c.Assert(err, IsNil)
	// Add another normal machine
	s.machine1, err = s.State.AddMachine("series", state.JobHostUnits)
	c.Assert(err, IsNil)

	// Create a machiner facades for machine 1.
	s.machiner = apiserver.NewMachiner(
		s.State,
		&fakeAuthorizer{
			tag:     state.MachineTag(s.machine1.Id()),
			manager: false,
		},
	)
}

func (s *machinerSuite) assertError(c *C, err *params.Error, code, messageRegexp string) {
	c.Assert(err, NotNil)
	c.Assert(api.ErrCode(err), Equals, code)
	c.Assert(err, ErrorMatches, messageRegexp)
}

func (s *machinerSuite) TestSetStatus(c *C) {
	err := s.machine0.SetStatus(params.StatusStarted, "blah")
	c.Assert(err, IsNil)
	err = s.machine1.SetStatus(params.StatusStopped, "foo")
	c.Assert(err, IsNil)

	args := params.MachinesSetStatus{
		Machines: []params.MachineSetStatus{
			{Id: "1", Status: params.StatusError, Info: "not really"},
			{Id: "0", Status: params.StatusStopped, Info: "foobar"},
			{Id: "42", Status: params.StatusStarted, Info: "blah"},
		}}
	result, err := s.machiner.SetStatus(args)
	c.Assert(err, IsNil)
	c.Assert(result.Errors, HasLen, 3)
	c.Assert(result.Errors[0], IsNil)
	s.assertError(c, result.Errors[1], api.CodeUnauthorized, "permission denied")
	s.assertError(c, result.Errors[2], api.CodeNotFound, "machine 42 not found")

	// Verify machine 0 - no change.
	status, info, err := s.machine0.Status()
	c.Assert(err, IsNil)
	c.Assert(status, Equals, params.StatusStarted)
	c.Assert(info, Equals, "blah")
	// ...machine 1 is fine though.
	status, info, err = s.machine1.Status()
	c.Assert(err, IsNil)
	c.Assert(status, Equals, params.StatusError)
	c.Assert(info, Equals, "not really")
}

func (s *machinerSuite) TestLife(c *C) {
	err := s.machine1.EnsureDead()
	c.Assert(err, IsNil)
	err = s.machine1.Refresh()
	c.Assert(err, IsNil)
	c.Assert(s.machine1.Life(), Equals, state.Dead)

	args := params.Machines{
		Ids: []string{"1", "0", "42"},
	}
	result, err := s.machiner.Life(args)
	c.Assert(err, IsNil)
	c.Assert(result.Machines, HasLen, 3)
	c.Assert(result.Machines[0].Error, IsNil)
	c.Assert(string(result.Machines[0].Life), Equals, "dead")
	s.assertError(c, result.Machines[1].Error, api.CodeUnauthorized, "permission denied")
	s.assertError(c, result.Machines[2].Error, api.CodeNotFound, "machine 42 not found")
}

func (s *machinerSuite) TestEnsureDead(c *C) {
	c.Assert(s.machine0.Life(), Equals, state.Alive)
	c.Assert(s.machine1.Life(), Equals, state.Alive)

	args := params.Machines{
		Ids: []string{"1", "0", "42"},
	}
	result, err := s.machiner.EnsureDead(args)
	c.Assert(err, IsNil)
	c.Assert(result.Errors, HasLen, 3)
	c.Assert(result.Errors[0], IsNil)
	s.assertError(c, result.Errors[1], api.CodeUnauthorized, "permission denied")
	s.assertError(c, result.Errors[2], api.CodeNotFound, "machine 42 not found")

	err = s.machine0.Refresh()
	c.Assert(err, IsNil)
	c.Assert(s.machine0.Life(), Equals, state.Alive)
	err = s.machine1.Refresh()
	c.Assert(err, IsNil)
	c.Assert(s.machine1.Life(), Equals, state.Dead)

	// Try it again on a Dead machine; should work.
	args = params.Machines{
		Ids: []string{"1"},
	}
	result, err = s.machiner.EnsureDead(args)
	c.Assert(err, IsNil)
	c.Assert(result.Errors, HasLen, 1)
	c.Assert(result.Errors[0], IsNil)

	// Verify Life is unchanged.
	err = s.machine1.Refresh()
	c.Assert(err, IsNil)
	c.Assert(s.machine1.Life(), Equals, state.Dead)
}