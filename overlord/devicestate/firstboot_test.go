// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2015-2018 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package devicestate_test

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "gopkg.in/check.v1"
	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/assertstest"
	"github.com/snapcore/snapd/boot/boottest"
	"github.com/snapcore/snapd/bootloader"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord"
	"github.com/snapcore/snapd/overlord/assertstate"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/configstate"
	"github.com/snapcore/snapd/overlord/configstate/config"
	"github.com/snapcore/snapd/overlord/devicestate"
	"github.com/snapcore/snapd/overlord/devicestate/devicestatetest"
	"github.com/snapcore/snapd/overlord/hookstate"
	"github.com/snapcore/snapd/overlord/ifacestate"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/seed/seedtest"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/snaptest"
	"github.com/snapcore/snapd/systemd"
	"github.com/snapcore/snapd/testutil"
	"github.com/snapcore/snapd/timings"
)

type FirstBootTestSuite struct {
	testutil.BaseTest

	systemctl *testutil.MockCmd

	// TestingSeed helps populating seeds (it provides
	// MakeAssertedSnap, WriteAssertions etc.) for tests.
	*seedtest.TestingSeed

	devAcct *asserts.Account

	overlord *overlord.Overlord

	perfTimings timings.Measurer
}

var _ = Suite(&FirstBootTestSuite{})

func (s *FirstBootTestSuite) SetUpTest(c *C) {
	s.BaseTest.SetUpTest(c)

	tempdir := c.MkDir()
	dirs.SetRootDir(tempdir)
	s.AddCleanup(func() { dirs.SetRootDir("/") })

	s.AddCleanup(release.MockOnClassic(false))

	// mock the world!
	err := os.MkdirAll(filepath.Join(dirs.SnapSeedDir, "snaps"), 0755)
	c.Assert(err, IsNil)
	err = os.MkdirAll(filepath.Join(dirs.SnapSeedDir, "assertions"), 0755)
	c.Assert(err, IsNil)

	err = os.MkdirAll(dirs.SnapServicesDir, 0755)
	c.Assert(err, IsNil)
	os.Setenv("SNAPPY_SQUASHFS_UNPACK_FOR_TESTS", "1")
	s.AddCleanup(func() { os.Unsetenv("SNAPPY_SQUASHFS_UNPACK_FOR_TESTS") })
	s.systemctl = testutil.MockCommand(c, "systemctl", "")
	s.AddCleanup(s.systemctl.Restore)

	err = ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), nil, 0644)
	c.Assert(err, IsNil)

	s.TestingSeed = &seedtest.TestingSeed{}
	s.SetupAssertSigning("can0nical", s)
	s.Brands.Register("my-brand", brandPrivKey, map[string]interface{}{
		"verification": "verified",
	})

	s.SnapsDir = filepath.Join(dirs.SnapSeedDir, "snaps")
	s.AssertsDir = filepath.Join(dirs.SnapSeedDir, "assertions")

	s.devAcct = assertstest.NewAccount(s.StoreSigning, "developer", map[string]interface{}{
		"account-id": "developerid",
	}, "")

	s.AddCleanup(ifacestate.MockSecurityBackends(nil))

	ovld, err := overlord.New(nil)
	c.Assert(err, IsNil)
	ovld.InterfaceManager().DisableUDevMonitor()
	s.overlord = ovld
	c.Assert(ovld.StartUp(), IsNil)

	// don't actually try to talk to the store on snapstate.Ensure
	// needs doing after the call to devicestate.Manager (which happens in overlord.New)
	snapstate.CanAutoRefresh = nil

	s.perfTimings = timings.New(nil)
}

func checkTrivialSeeding(c *C, tsAll []*state.TaskSet) {
	// run internal core config and  mark seeded
	c.Check(tsAll, HasLen, 2)
	tasks := tsAll[0].Tasks()
	c.Check(tasks, HasLen, 1)
	c.Assert(tasks[0].Kind(), Equals, "run-hook")
	var hooksup hookstate.HookSetup
	err := tasks[0].Get("hook-setup", &hooksup)
	c.Assert(err, IsNil)
	c.Check(hooksup.Hook, Equals, "configure")
	c.Check(hooksup.Snap, Equals, "core")
	tasks = tsAll[1].Tasks()
	c.Check(tasks, HasLen, 1)
	c.Check(tasks[0].Kind(), Equals, "mark-seeded")
}

func (s *FirstBootTestSuite) modelHeaders(modelStr string, reqSnaps ...string) map[string]interface{} {
	headers := map[string]interface{}{
		"architecture": "amd64",
		"store":        "canonical",
	}
	if strings.HasSuffix(modelStr, "-classic") {
		headers["classic"] = "true"
	} else {
		headers["kernel"] = "pc-kernel"
		headers["gadget"] = "pc"
	}
	if len(reqSnaps) != 0 {
		reqs := make([]interface{}, len(reqSnaps))
		for i, req := range reqSnaps {
			reqs[i] = req
		}
		headers["required-snaps"] = reqs
	}
	return headers
}

func (s *FirstBootTestSuite) makeModelAssertionChain(c *C, modName string, extraHeaders map[string]interface{}, reqSnaps ...string) []asserts.Assertion {
	return s.MakeModelAssertionChain("my-brand", modName, s.modelHeaders(modName, reqSnaps...), extraHeaders)
}

func (s *FirstBootTestSuite) TestPopulateFromSeedOnClassicNoop(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()

	err := os.Remove(filepath.Join(dirs.SnapSeedDir, "assertions"))
	c.Assert(err, IsNil)

	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)
	checkTrivialSeeding(c, tsAll)

	// already set the fallback model

	// verify that the model was added
	db := assertstate.DB(st)
	as, err := db.Find(asserts.ModelType, map[string]string{
		"series":   "16",
		"brand-id": "generic",
		"model":    "generic-classic",
	})
	c.Assert(err, IsNil)
	_, ok := as.(*asserts.Model)
	c.Check(ok, Equals, true)

	ds, err := devicestatetest.Device(st)
	c.Assert(err, IsNil)
	c.Check(ds.Brand, Equals, "generic")
	c.Check(ds.Model, Equals, "generic-classic")
}

func (s *FirstBootTestSuite) TestPopulateFromSeedOnClassicNoSeedYaml(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	ovld, err := overlord.New(nil)
	c.Assert(err, IsNil)
	st := ovld.State()

	// add the model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model-classic", nil)
	s.WriteAssertions("model.asserts", assertsChain...)

	err = os.Remove(filepath.Join(dirs.SnapSeedDir, "seed.yaml"))
	c.Assert(err, IsNil)

	st.Lock()
	defer st.Unlock()

	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)
	checkTrivialSeeding(c, tsAll)

	ds, err := devicestatetest.Device(st)
	c.Assert(err, IsNil)
	c.Check(ds.Brand, Equals, "my-brand")
	c.Check(ds.Model, Equals, "my-model-classic")
}

func (s *FirstBootTestSuite) TestPopulateFromSeedOnClassicEmptySeedYaml(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	ovld, err := overlord.New(nil)
	c.Assert(err, IsNil)
	st := ovld.State()

	// add the model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model-classic", nil)
	s.WriteAssertions("model.asserts", assertsChain...)

	// create an empty seed.yaml
	err = ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), nil, 0644)
	c.Assert(err, IsNil)

	st.Lock()
	defer st.Unlock()

	_, err = devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, ErrorMatches, "cannot proceed, no snaps to seed")
}

func (s *FirstBootTestSuite) TestPopulateFromSeedOnClassicNoSeedYamlWithCloudInstanceData(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	st := s.overlord.State()

	// add the model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model-classic", nil)
	s.WriteAssertions("model.asserts", assertsChain...)

	err := os.Remove(filepath.Join(dirs.SnapSeedDir, "seed.yaml"))
	c.Assert(err, IsNil)

	// write cloud instance data
	const instData = `{
 "v1": {
  "availability-zone": "us-east-2b",
  "cloud-name": "aws",
  "instance-id": "i-03bdbe0d89f4c8ec9",
  "local-hostname": "ip-10-41-41-143",
  "region": "us-east-2"
 }
}`
	err = os.MkdirAll(filepath.Dir(dirs.CloudInstanceDataFile), 0755)
	c.Assert(err, IsNil)
	err = ioutil.WriteFile(dirs.CloudInstanceDataFile, []byte(instData), 0600)
	c.Assert(err, IsNil)

	st.Lock()
	defer st.Unlock()

	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)
	checkTrivialSeeding(c, tsAll)

	ds, err := devicestatetest.Device(st)
	c.Assert(err, IsNil)
	c.Check(ds.Brand, Equals, "my-brand")
	c.Check(ds.Model, Equals, "my-model-classic")

	// now run the change and check the result
	// use the expected kind otherwise settle will start another one
	chg := st.NewChange("seed", "run the populate from seed changes")
	for _, ts := range tsAll {
		chg.AddAll(ts)
	}
	c.Assert(st.Changes(), HasLen, 1)

	// avoid device reg
	chg1 := st.NewChange("become-operational", "init device")
	chg1.SetStatus(state.DoingStatus)

	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(chg.Err(), IsNil)
	c.Assert(err, IsNil)

	// check marked seeded
	var seeded bool
	err = st.Get("seeded", &seeded)
	c.Assert(err, IsNil)
	c.Check(seeded, Equals, true)

	// check captured cloud information
	tr := config.NewTransaction(st)
	var cloud auth.CloudInfo
	err = tr.Get("core", "cloud", &cloud)
	c.Assert(err, IsNil)
	c.Check(cloud.Name, Equals, "aws")
	c.Check(cloud.Region, Equals, "us-east-2")
	c.Check(cloud.AvailabilityZone, Equals, "us-east-2b")
}

func (s *FirstBootTestSuite) TestPopulateFromSeedErrorsOnState(c *C) {
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()
	st.Set("seeded", true)

	_, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, ErrorMatches, "cannot populate state: already seeded")
}

func (s *FirstBootTestSuite) makeCoreSnaps(c *C, extraGadgetYaml string) (coreFname, kernelFname, gadgetFname string) {
	files := [][]string{}
	if strings.Contains(extraGadgetYaml, "defaults:") {
		files = [][]string{{"meta/hooks/configure", ""}}
	}

	// put core snap into the SnapBlobDir
	snapYaml := `name: core
version: 1.0
type: os`
	coreFname, coreDecl, coreRev := s.MakeAssertedSnap(c, snapYaml, files, snap.R(1), "canonical")
	s.WriteAssertions("core.asserts", coreRev, coreDecl)

	// put kernel snap into the SnapBlobDir
	snapYaml = `name: pc-kernel
version: 1.0
type: kernel`
	kernelFname, kernelDecl, kernelRev := s.MakeAssertedSnap(c, snapYaml, files, snap.R(1), "canonical")
	s.WriteAssertions("kernel.asserts", kernelRev, kernelDecl)

	gadgetYaml := `
volumes:
    volume-id:
        bootloader: grub
`
	gadgetYaml += extraGadgetYaml

	// put gadget snap into the SnapBlobDir
	files = append(files, []string{"meta/gadget.yaml", gadgetYaml})

	snapYaml = `name: pc
version: 1.0
type: gadget`
	gadgetFname, gadgetDecl, gadgetRev := s.MakeAssertedSnap(c, snapYaml, files, snap.R(1), "canonical")
	s.WriteAssertions("gadget.asserts", gadgetRev, gadgetDecl)

	return coreFname, kernelFname, gadgetFname
}

func checkOrder(c *C, tsAll []*state.TaskSet, snaps ...string) {
	matched := 0
	var prevTask *state.Task
	for i, ts := range tsAll {
		task0 := ts.Tasks()[0]
		waitTasks := task0.WaitTasks()
		if i == 0 {
			c.Check(waitTasks, HasLen, 0)
		} else {
			c.Check(waitTasks, testutil.Contains, prevTask)
		}
		prevTask = task0
		if task0.Kind() != "prerequisites" {
			continue
		}
		snapsup, err := snapstate.TaskSnapSetup(task0)
		c.Assert(err, IsNil, Commentf("%#v", task0))
		c.Check(snapsup.InstanceName(), Equals, snaps[matched])
		matched++
	}
	c.Check(matched, Equals, len(snaps))
}

func checkSeedTasks(c *C, tsAll []*state.TaskSet) {
	// the tasks of the last taskset must be gadget-connect, mark-seeded
	lastTasks := tsAll[len(tsAll)-1].Tasks()
	c.Check(lastTasks, HasLen, 2)
	gadgetConnectTask := lastTasks[0]
	markSeededTask := lastTasks[1]
	c.Check(gadgetConnectTask.Kind(), Equals, "gadget-connect")
	c.Check(markSeededTask.Kind(), Equals, "mark-seeded")
	// and the gadget-connect must wait for the other tasks
	prevTasks := tsAll[len(tsAll)-2].Tasks()
	otherTask := prevTasks[len(prevTasks)-1]
	c.Check(gadgetConnectTask.WaitTasks(), testutil.Contains, otherTask)
	// add the mark-seeded waits for gadget-connects
	c.Check(markSeededTask.WaitTasks(), DeepEquals, []*state.Task{gadgetConnectTask})
}

func (s *FirstBootTestSuite) makeSeedChange(c *C, st *state.State) *state.Change {
	coreFname, kernelFname, gadgetFname := s.makeCoreSnaps(c, "")

	s.WriteAssertions("developer.account", s.devAcct)

	// put a firstboot snap into the SnapBlobDir
	snapYaml := `name: foo
version: 1.0`
	fooFname, fooDecl, fooRev := s.MakeAssertedSnap(c, snapYaml, nil, snap.R(128), "developerid")
	s.WriteAssertions("foo.snap-declaration", fooDecl)
	s.WriteAssertions("foo.snap-revision", fooRev)

	// put a firstboot local snap into the SnapBlobDir
	snapYaml = `name: local
version: 1.0`
	mockSnapFile := snaptest.MakeTestSnapWithFiles(c, snapYaml, nil)
	targetSnapFile2 := filepath.Join(dirs.SnapSeedDir, "snaps", filepath.Base(mockSnapFile))
	err := os.Rename(mockSnapFile, targetSnapFile2)
	c.Assert(err, IsNil)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", nil, "foo")
	for i, as := range assertsChain {
		s.WriteAssertions(strconv.Itoa(i), as)
	}

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: core
   file: %s
 - name: pc-kernel
   file: %s
 - name: pc
   file: %s
 - name: foo
   file: %s
   devmode: true
   contact: mailto:some.guy@example.com
 - name: local
   unasserted: true
   file: %s
`, coreFname, kernelFname, gadgetFname, fooFname, filepath.Base(targetSnapFile2)))
	err = ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st.Lock()
	defer st.Unlock()
	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)

	checkOrder(c, tsAll, "core", "pc-kernel", "pc", "foo", "local")
	checkSeedTasks(c, tsAll)

	// now run the change and check the result
	// use the expected kind otherwise settle with start another one
	chg := st.NewChange("seed", "run the populate from seed changes")
	for _, ts := range tsAll {
		chg.AddAll(ts)
	}
	c.Assert(st.Changes(), HasLen, 1)

	// avoid device reg
	chg1 := st.NewChange("become-operational", "init device")
	chg1.SetStatus(state.DoingStatus)

	return chg
}

func (s *FirstBootTestSuite) TestPopulateFromSeedHappy(c *C) {
	loader := boottest.NewMockBootloader("mock", c.MkDir())
	bootloader.Force(loader)
	defer bootloader.Force(nil)
	boottest.SetBootKernel("pc-kernel_1.snap", loader)
	boottest.SetBootBase("core_1.snap", loader)

	st := s.overlord.State()
	chg := s.makeSeedChange(c, st)
	err := s.overlord.Settle(settleTimeout)
	c.Assert(err, IsNil)

	st.Lock()
	defer st.Unlock()

	c.Assert(chg.Err(), IsNil)

	// and check the snap got correctly installed
	c.Check(osutil.FileExists(filepath.Join(dirs.SnapMountDir, "foo", "128", "meta", "snap.yaml")), Equals, true)

	c.Check(osutil.FileExists(filepath.Join(dirs.SnapMountDir, "local", "x1", "meta", "snap.yaml")), Equals, true)

	// verify
	r, err := os.Open(dirs.SnapStateFile)
	c.Assert(err, IsNil)
	state, err := state.ReadState(nil, r)
	c.Assert(err, IsNil)

	state.Lock()
	defer state.Unlock()
	// check core, kernel, gadget
	_, err = snapstate.CurrentInfo(state, "core")
	c.Assert(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "pc-kernel")
	c.Assert(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "pc")
	c.Assert(err, IsNil)

	// ensure required flag is set on all essential snaps
	var snapst snapstate.SnapState
	for _, reqName := range []string{"core", "pc-kernel", "pc"} {
		err = snapstate.Get(state, reqName, &snapst)
		c.Assert(err, IsNil)
		c.Assert(snapst.Required, Equals, true, Commentf("required not set for %v", reqName))
	}

	// check foo
	info, err := snapstate.CurrentInfo(state, "foo")
	c.Assert(err, IsNil)
	c.Assert(info.SnapID, Equals, "foodidididididididididididididid")
	c.Assert(info.Revision, Equals, snap.R(128))
	c.Assert(info.Contact, Equals, "mailto:some.guy@example.com")
	pubAcct, err := assertstate.Publisher(st, info.SnapID)
	c.Assert(err, IsNil)
	c.Check(pubAcct.AccountID(), Equals, "developerid")

	err = snapstate.Get(state, "foo", &snapst)
	c.Assert(err, IsNil)
	c.Assert(snapst.DevMode, Equals, true)
	c.Assert(snapst.Required, Equals, true)

	// check local
	info, err = snapstate.CurrentInfo(state, "local")
	c.Assert(err, IsNil)
	c.Assert(info.SnapID, Equals, "")
	c.Assert(info.Revision, Equals, snap.R("x1"))

	var snapst2 snapstate.SnapState
	err = snapstate.Get(state, "local", &snapst2)
	c.Assert(err, IsNil)
	c.Assert(snapst2.Required, Equals, false)

	// and ensure state is now considered seeded
	var seeded bool
	err = state.Get("seeded", &seeded)
	c.Assert(err, IsNil)
	c.Check(seeded, Equals, true)

	// check we set seed-time
	var seedTime time.Time
	err = state.Get("seed-time", &seedTime)
	c.Assert(err, IsNil)
	c.Check(seedTime.IsZero(), Equals, false)
}

func (s *FirstBootTestSuite) TestPopulateFromSeedMissingBootloader(c *C) {
	st0 := s.overlord.State()
	st0.Lock()
	db := assertstate.DB(st0)
	st0.Unlock()

	// we run only with the relevant managers to produce the error
	// situation
	o := overlord.Mock()
	st := o.State()
	snapmgr, err := snapstate.Manager(st, o.TaskRunner())
	c.Assert(err, IsNil)
	o.AddManager(snapmgr)

	ifacemgr, err := ifacestate.Manager(st, nil, o.TaskRunner(), nil, nil)
	c.Assert(err, IsNil)
	o.AddManager(ifacemgr)
	c.Assert(o.StartUp(), IsNil)

	st.Lock()
	assertstate.ReplaceDB(st, db.(*asserts.Database))
	st.Unlock()

	o.AddManager(o.TaskRunner())

	chg := s.makeSeedChange(c, st)

	se := o.StateEngine()
	// we cannot use Settle because the Change will not become Clean
	// under the subset of managers
	for i := 0; i < 25 && !chg.IsReady(); i++ {
		se.Ensure()
		se.Wait()
	}

	st.Lock()
	defer st.Unlock()
	c.Assert(chg.Err(), ErrorMatches, `(?s).* cannot determine bootloader.*`)
}

func (s *FirstBootTestSuite) TestPopulateFromSeedHappyMultiAssertsFiles(c *C) {
	loader := boottest.NewMockBootloader("mock", c.MkDir())
	bootloader.Force(loader)
	defer bootloader.Force(nil)
	boottest.SetBootKernel("pc-kernel_1.snap", loader)
	boottest.SetBootBase("core_1.snap", loader)

	coreFname, kernelFname, gadgetFname := s.makeCoreSnaps(c, "")

	// put a firstboot snap into the SnapBlobDir
	snapYaml := `name: foo
version: 1.0`
	fooFname, fooDecl, fooRev := s.MakeAssertedSnap(c, snapYaml, nil, snap.R(128), "developerid")
	s.WriteAssertions("foo.asserts", s.devAcct, fooRev, fooDecl)

	// put a 2nd firstboot snap into the SnapBlobDir
	snapYaml = `name: bar
version: 1.0`
	barFname, barDecl, barRev := s.MakeAssertedSnap(c, snapYaml, nil, snap.R(65), "developerid")
	s.WriteAssertions("bar.asserts", s.devAcct, barDecl, barRev)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", nil)
	s.WriteAssertions("model.asserts", assertsChain...)

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: core
   file: %s
 - name: pc-kernel
   file: %s
 - name: pc
   file: %s
 - name: foo
   file: %s
 - name: bar
   file: %s
`, coreFname, kernelFname, gadgetFname, fooFname, barFname))
	err := ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()

	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)
	// use the expected kind otherwise settle with start another one
	chg := st.NewChange("seed", "run the populate from seed changes")
	for _, ts := range tsAll {
		chg.AddAll(ts)
	}
	c.Assert(st.Changes(), HasLen, 1)

	// avoid device reg
	chg1 := st.NewChange("become-operational", "init device")
	chg1.SetStatus(state.DoingStatus)

	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(chg.Err(), IsNil)
	c.Assert(err, IsNil)

	// and check the snap got correctly installed
	c.Check(osutil.FileExists(filepath.Join(dirs.SnapMountDir, "foo", "128", "meta", "snap.yaml")), Equals, true)

	// and check the snap got correctly installed
	c.Check(osutil.FileExists(filepath.Join(dirs.SnapMountDir, "bar", "65", "meta", "snap.yaml")), Equals, true)

	// verify
	r, err := os.Open(dirs.SnapStateFile)
	c.Assert(err, IsNil)
	state, err := state.ReadState(nil, r)
	c.Assert(err, IsNil)

	state.Lock()
	defer state.Unlock()
	// check foo
	info, err := snapstate.CurrentInfo(state, "foo")
	c.Assert(err, IsNil)
	c.Check(info.SnapID, Equals, "foodidididididididididididididid")
	c.Check(info.Revision, Equals, snap.R(128))
	pubAcct, err := assertstate.Publisher(st, info.SnapID)
	c.Assert(err, IsNil)
	c.Check(pubAcct.AccountID(), Equals, "developerid")

	// check bar
	info, err = snapstate.CurrentInfo(state, "bar")
	c.Assert(err, IsNil)
	c.Check(info.SnapID, Equals, "bardidididididididididididididid")
	c.Check(info.Revision, Equals, snap.R(65))
	pubAcct, err = assertstate.Publisher(st, info.SnapID)
	c.Assert(err, IsNil)
	c.Check(pubAcct.AccountID(), Equals, "developerid")
}

func (s *FirstBootTestSuite) TestPopulateFromSeedConfigureHappy(c *C) {
	loader := boottest.NewMockBootloader("mock", c.MkDir())
	bootloader.Force(loader)
	defer bootloader.Force(nil)
	boottest.SetBootKernel("pc-kernel_1.snap", loader)
	boottest.SetBootBase("core_1.snap", loader)

	const defaultsYaml = `
defaults:
    foodidididididididididididididid:
       foo-cfg: foo.
    coreidididididididididididididid:
       core-cfg: core_cfg_defl
    pckernelidididididididididididid:
       pc-kernel-cfg: pc-kernel_cfg_defl
    pcididididididididididididididid:
       pc-cfg: pc_cfg_defl
`
	coreFname, kernelFname, gadgetFname := s.makeCoreSnaps(c, defaultsYaml)

	s.WriteAssertions("developer.account", s.devAcct)

	// put a firstboot snap into the SnapBlobDir
	files := [][]string{{"meta/hooks/configure", ""}}
	snapYaml := `name: foo
version: 1.0`
	fooFname, fooDecl, fooRev := s.MakeAssertedSnap(c, snapYaml, files, snap.R(128), "developerid")
	s.WriteAssertions("foo.asserts", fooDecl, fooRev)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", nil, "foo")
	s.WriteAssertions("model.asserts", assertsChain...)

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: core
   file: %s
 - name: pc-kernel
   file: %s
 - name: pc
   file: %s
 - name: foo
   file: %s
`, coreFname, kernelFname, gadgetFname, fooFname))
	err := ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()
	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)

	checkSeedTasks(c, tsAll)

	// now run the change and check the result
	// use the expected kind otherwise settle with start another one
	chg := st.NewChange("seed", "run the populate from seed changes")
	for _, ts := range tsAll {
		chg.AddAll(ts)
	}
	c.Assert(st.Changes(), HasLen, 1)

	var configured []string
	hookInvoke := func(ctx *hookstate.Context, tomb *tomb.Tomb) ([]byte, error) {
		ctx.Lock()
		defer ctx.Unlock()
		// we have a gadget at this point(s)
		ok, err := snapstate.HasSnapOfType(st, snap.TypeGadget)
		c.Check(err, IsNil)
		c.Check(ok, Equals, true)
		configured = append(configured, ctx.InstanceName())
		return nil, nil
	}

	rhk := hookstate.MockRunHook(hookInvoke)
	defer rhk()

	// ensure we have something that captures the core config
	restore := configstate.MockConfigcoreRun(func(config.Conf) error {
		configured = append(configured, "configcore")
		return nil
	})
	defer restore()

	// avoid device reg
	chg1 := st.NewChange("become-operational", "init device")
	chg1.SetStatus(state.DoingStatus)

	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(chg.Err(), IsNil)
	c.Assert(err, IsNil)

	// and check the snap got correctly installed
	c.Check(osutil.FileExists(filepath.Join(dirs.SnapMountDir, "foo", "128", "meta", "snap.yaml")), Equals, true)

	// verify
	r, err := os.Open(dirs.SnapStateFile)
	c.Assert(err, IsNil)
	state, err := state.ReadState(nil, r)
	c.Assert(err, IsNil)

	state.Lock()
	defer state.Unlock()
	tr := config.NewTransaction(state)
	var val string

	// check core, kernel, gadget
	_, err = snapstate.CurrentInfo(state, "core")
	c.Assert(err, IsNil)
	err = tr.Get("core", "core-cfg", &val)
	c.Assert(err, IsNil)
	c.Check(val, Equals, "core_cfg_defl")

	_, err = snapstate.CurrentInfo(state, "pc-kernel")
	c.Assert(err, IsNil)
	err = tr.Get("pc-kernel", "pc-kernel-cfg", &val)
	c.Assert(err, IsNil)
	c.Check(val, Equals, "pc-kernel_cfg_defl")

	_, err = snapstate.CurrentInfo(state, "pc")
	c.Assert(err, IsNil)
	err = tr.Get("pc", "pc-cfg", &val)
	c.Assert(err, IsNil)
	c.Check(val, Equals, "pc_cfg_defl")

	// check foo
	info, err := snapstate.CurrentInfo(state, "foo")
	c.Assert(err, IsNil)
	c.Assert(info.SnapID, Equals, "foodidididididididididididididid")
	c.Assert(info.Revision, Equals, snap.R(128))
	pubAcct, err := assertstate.Publisher(st, info.SnapID)
	c.Assert(err, IsNil)
	c.Check(pubAcct.AccountID(), Equals, "developerid")

	// check foo config
	err = tr.Get("foo", "foo-cfg", &val)
	c.Assert(err, IsNil)
	c.Check(val, Equals, "foo.")

	c.Check(configured, DeepEquals, []string{"configcore", "pc-kernel", "pc", "foo"})

	// and ensure state is now considered seeded
	var seeded bool
	err = state.Get("seeded", &seeded)
	c.Assert(err, IsNil)
	c.Check(seeded, Equals, true)
}

func (s *FirstBootTestSuite) TestPopulateFromSeedGadgetConnectHappy(c *C) {
	loader := boottest.NewMockBootloader("mock", c.MkDir())
	bootloader.Force(loader)
	defer bootloader.Force(nil)
	boottest.SetBootKernel("pc-kernel_1.snap", loader)
	boottest.SetBootBase("core_1.snap", loader)

	const connectionsYaml = `
connections:
  - plug: foodidididididididididididididid:network-control
`
	coreFname, kernelFname, gadgetFname := s.makeCoreSnaps(c, connectionsYaml)

	s.WriteAssertions("developer.account", s.devAcct)

	snapYaml := `name: foo
version: 1.0
plugs:
  network-control:
`
	fooFname, fooDecl, fooRev := s.MakeAssertedSnap(c, snapYaml, nil, snap.R(128), "developerid")
	s.WriteAssertions("foo.asserts", fooDecl, fooRev)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", nil, "foo")
	s.WriteAssertions("model.asserts", assertsChain...)

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: core
   file: %s
 - name: pc-kernel
   file: %s
 - name: pc
   file: %s
 - name: foo
   file: %s
`, coreFname, kernelFname, gadgetFname, fooFname))
	err := ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()
	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)

	checkSeedTasks(c, tsAll)

	// now run the change and check the result
	// use the expected kind otherwise settle with start another one
	chg := st.NewChange("seed", "run the populate from seed changes")
	for _, ts := range tsAll {
		chg.AddAll(ts)
	}
	c.Assert(st.Changes(), HasLen, 1)

	// avoid device reg
	chg1 := st.NewChange("become-operational", "init device")
	chg1.SetStatus(state.DoingStatus)

	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(chg.Err(), IsNil)
	c.Assert(err, IsNil)

	// and check the snap got correctly installed
	c.Check(osutil.FileExists(filepath.Join(dirs.SnapMountDir, "foo", "128", "meta", "snap.yaml")), Equals, true)

	// verify
	r, err := os.Open(dirs.SnapStateFile)
	c.Assert(err, IsNil)
	state, err := state.ReadState(nil, r)
	c.Assert(err, IsNil)

	state.Lock()
	defer state.Unlock()

	// check foo
	info, err := snapstate.CurrentInfo(state, "foo")
	c.Assert(err, IsNil)
	c.Assert(info.SnapID, Equals, "foodidididididididididididididid")
	c.Assert(info.Revision, Equals, snap.R(128))
	pubAcct, err := assertstate.Publisher(st, info.SnapID)
	c.Assert(err, IsNil)
	c.Check(pubAcct.AccountID(), Equals, "developerid")

	// check connection
	var conns map[string]interface{}
	err = state.Get("conns", &conns)
	c.Assert(err, IsNil)
	c.Check(conns, HasLen, 1)
	c.Check(conns, DeepEquals, map[string]interface{}{
		"foo:network-control core:network-control": map[string]interface{}{
			"interface": "network-control", "auto": true, "by-gadget": true,
		},
	})

	// and ensure state is now considered seeded
	var seeded bool
	err = state.Get("seeded", &seeded)
	c.Assert(err, IsNil)
	c.Check(seeded, Equals, true)
}

func (s *FirstBootTestSuite) TestImportAssertionsFromSeedClassicModelMismatch(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	ovld, err := overlord.New(nil)
	c.Assert(err, IsNil)
	st := ovld.State()

	// add the odel assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", nil)
	s.WriteAssertions("model.asserts", assertsChain...)

	// import them
	st.Lock()
	defer st.Unlock()

	_, err = devicestate.ImportAssertionsFromSeed(st)
	c.Assert(err, ErrorMatches, "cannot seed a classic system with an all-snaps model")
}

func (s *FirstBootTestSuite) TestImportAssertionsFromSeedAllSnapsModelMismatch(c *C) {
	ovld, err := overlord.New(nil)
	c.Assert(err, IsNil)
	st := ovld.State()

	// add the model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model-classic", nil)
	s.WriteAssertions("model.asserts", assertsChain...)

	// import them
	st.Lock()
	defer st.Unlock()

	_, err = devicestate.ImportAssertionsFromSeed(st)
	c.Assert(err, ErrorMatches, "cannot seed an all-snaps system with a classic model")
}

func (s *FirstBootTestSuite) TestImportAssertionsFromSeedHappy(c *C) {
	ovld, err := overlord.New(nil)
	c.Assert(err, IsNil)
	st := ovld.State()

	// add a bunch of assertions (model assertion and its chain)
	assertsChain := s.makeModelAssertionChain(c, "my-model", nil)
	for i, as := range assertsChain {
		fname := strconv.Itoa(i)
		if as.Type() == asserts.ModelType {
			fname = "model"
		}
		s.WriteAssertions(fname, as)
	}

	// import them
	st.Lock()
	defer st.Unlock()

	model, err := devicestate.ImportAssertionsFromSeed(st)
	c.Assert(err, IsNil)
	c.Assert(model, NotNil)

	// verify that the model was added
	db := assertstate.DB(st)
	as, err := db.Find(asserts.ModelType, map[string]string{
		"series":   "16",
		"brand-id": "my-brand",
		"model":    "my-model",
	})
	c.Assert(err, IsNil)
	_, ok := as.(*asserts.Model)
	c.Check(ok, Equals, true)

	ds, err := devicestatetest.Device(st)
	c.Assert(err, IsNil)
	c.Check(ds.Brand, Equals, "my-brand")
	c.Check(ds.Model, Equals, "my-model")

	c.Check(model.BrandID(), Equals, "my-brand")
	c.Check(model.Model(), Equals, "my-model")
}

func (s *FirstBootTestSuite) TestImportAssertionsFromSeedMissingSig(c *C) {
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()

	// write out only the model assertion
	assertsChain := s.makeModelAssertionChain(c, "my-model", nil)
	for _, as := range assertsChain {
		if as.Type() == asserts.ModelType {
			s.WriteAssertions("model", as)
			break
		}
	}

	// try import and verify that its rejects because other assertions are
	// missing
	_, err := devicestate.ImportAssertionsFromSeed(st)
	c.Assert(err, ErrorMatches, "cannot resolve prerequisite assertion: account-key .*")
}

func (s *FirstBootTestSuite) TestImportAssertionsFromSeedTwoModelAsserts(c *C) {
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()

	// write out two model assertions
	model := s.Brands.Model("my-brand", "my-model", s.modelHeaders("my-model"))
	s.WriteAssertions("model", model)

	model2 := s.Brands.Model("my-brand", "my-second-model", s.modelHeaders("my-second-model"))
	s.WriteAssertions("model2", model2)

	// try import and verify that its rejects because other assertions are
	// missing
	_, err := devicestate.ImportAssertionsFromSeed(st)
	c.Assert(err, ErrorMatches, "cannot add more than one model assertion")
}

func (s *FirstBootTestSuite) TestImportAssertionsFromSeedNoModelAsserts(c *C) {
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()

	assertsChain := s.makeModelAssertionChain(c, "my-model", nil)
	for i, as := range assertsChain {
		if as.Type() != asserts.ModelType {
			s.WriteAssertions(strconv.Itoa(i), as)
		}
	}

	// try import and verify that its rejects because other assertions are
	// missing
	_, err := devicestate.ImportAssertionsFromSeed(st)
	c.Assert(err, ErrorMatches, "need a model assertion")
}

type core18SnapsOpts struct {
	classic bool
	gadget  bool
}

func (s *FirstBootTestSuite) makeCore18Snaps(c *C, opts *core18SnapsOpts) (core18Fn, snapdFn, kernelFn, gadgetFn string) {
	if opts == nil {
		opts = &core18SnapsOpts{}
	}

	files := [][]string{}

	core18Yaml := `name: core18
version: 1.0
type: base`
	core18Fname, core18Decl, core18Rev := s.MakeAssertedSnap(c, core18Yaml, files, snap.R(1), "canonical")
	s.WriteAssertions("core18.asserts", core18Rev, core18Decl)

	snapdYaml := `name: snapd
version: 1.0
`
	snapdFname, snapdDecl, snapdRev := s.MakeAssertedSnap(c, snapdYaml, nil, snap.R(2), "canonical")
	s.WriteAssertions("snapd.asserts", snapdRev, snapdDecl)

	var kernelFname string
	if !opts.classic {
		kernelYaml := `name: pc-kernel
version: 1.0
type: kernel`
		fname, kernelDecl, kernelRev := s.MakeAssertedSnap(c, kernelYaml, files, snap.R(1), "canonical")
		s.WriteAssertions("kernel.asserts", kernelRev, kernelDecl)
		kernelFname = fname
	}

	if !opts.classic {
		gadgetYaml := `
volumes:
    volume-id:
        bootloader: grub
`
		files = append(files, []string{"meta/gadget.yaml", gadgetYaml})
	}

	var gadgetFname string
	if !opts.classic || opts.gadget {
		gaYaml := `name: pc
version: 1.0
type: gadget
base: core18
`
		fname, gadgetDecl, gadgetRev := s.MakeAssertedSnap(c, gaYaml, files, snap.R(1), "canonical")
		s.WriteAssertions("gadget.asserts", gadgetRev, gadgetDecl)
		gadgetFname = fname
	}

	return core18Fname, snapdFname, kernelFname, gadgetFname
}

func (s *FirstBootTestSuite) TestPopulateFromSeedWithBaseHappy(c *C) {
	var sysdLog [][]string
	systemctlRestorer := systemd.MockSystemctl(func(cmd ...string) ([]byte, error) {
		sysdLog = append(sysdLog, cmd)
		return []byte("ActiveState=inactive\n"), nil
	})
	defer systemctlRestorer()

	loader := boottest.NewMockBootloader("mock", c.MkDir())
	bootloader.Force(loader)
	defer bootloader.Force(nil)
	boottest.SetBootKernel("pc-kernel_1.snap", loader)
	boottest.SetBootBase("core18_1.snap", loader)

	core18Fname, snapdFname, kernelFname, gadgetFname := s.makeCore18Snaps(c, nil)

	s.WriteAssertions("developer.account", s.devAcct)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", map[string]interface{}{"base": "core18"})
	s.WriteAssertions("model.asserts", assertsChain...)

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: snapd
   file: %s
 - name: core18
   file: %s
 - name: pc-kernel
   file: %s
 - name: pc
   file: %s
`, snapdFname, core18Fname, kernelFname, gadgetFname))
	err := ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()
	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)

	checkOrder(c, tsAll, "snapd", "core18", "pc-kernel", "pc")

	// now run the change and check the result
	// use the expected kind otherwise settle with start another one
	chg := st.NewChange("seed", "run the populate from seed changes")
	for _, ts := range tsAll {
		chg.AddAll(ts)
	}
	c.Assert(st.Changes(), HasLen, 1)

	c.Assert(chg.Err(), IsNil)

	// avoid device reg
	chg1 := st.NewChange("become-operational", "init device")
	chg1.SetStatus(state.DoingStatus)

	// run change until it wants to restart
	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(err, IsNil)

	// at this point the system is "restarting", pretend the restart has
	// happened
	c.Assert(chg.Status(), Equals, state.DoingStatus)
	state.MockRestarting(st, state.RestartUnset)
	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(err, IsNil)
	c.Assert(chg.Status(), Equals, state.DoneStatus)

	// verify
	r, err := os.Open(dirs.SnapStateFile)
	c.Assert(err, IsNil)
	state, err := state.ReadState(nil, r)
	c.Assert(err, IsNil)

	state.Lock()
	defer state.Unlock()
	// check snapd, core18, kernel, gadget
	_, err = snapstate.CurrentInfo(state, "snapd")
	c.Check(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "core18")
	c.Check(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "pc-kernel")
	c.Check(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "pc")
	c.Check(err, IsNil)

	// ensure required flag is set on all essential snaps
	var snapst snapstate.SnapState
	for _, reqName := range []string{"snapd", "core18", "pc-kernel", "pc"} {
		err = snapstate.Get(state, reqName, &snapst)
		c.Assert(err, IsNil)
		c.Assert(snapst.Required, Equals, true, Commentf("required not set for %v", reqName))
	}

	// the right systemd commands were run
	c.Check(sysdLog, testutil.DeepContains, []string{"start", "usr-lib-snapd.mount"})

	// and ensure state is now considered seeded
	var seeded bool
	err = state.Get("seeded", &seeded)
	c.Assert(err, IsNil)
	c.Check(seeded, Equals, true)

	// check we set seed-time
	var seedTime time.Time
	err = state.Get("seed-time", &seedTime)
	c.Assert(err, IsNil)
	c.Check(seedTime.IsZero(), Equals, false)
}

func (s *FirstBootTestSuite) TestPopulateFromSeedOrdering(c *C) {
	s.WriteAssertions("developer.account", s.devAcct)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", map[string]interface{}{"base": "core18"})
	s.WriteAssertions("model.asserts", assertsChain...)

	core18Fname, snapdFname, kernelFname, gadgetFname := s.makeCore18Snaps(c, nil)

	snapYaml := `name: snap-req-other-base
version: 1.0
base: other-base
`
	snapFname, snapDecl, snapRev := s.MakeAssertedSnap(c, snapYaml, nil, snap.R(128), "developerid")
	s.WriteAssertions("snap-req-other-base.asserts", s.devAcct, snapRev, snapDecl)
	baseYaml := `name: other-base
version: 1.0
type: base
`
	baseFname, baseDecl, baseRev := s.MakeAssertedSnap(c, baseYaml, nil, snap.R(127), "developerid")
	s.WriteAssertions("other-base.asserts", s.devAcct, baseRev, baseDecl)

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: snapd
   file: %s
 - name: core18
   file: %s
 - name: pc-kernel
   file: %s
 - name: pc
   file: %s
 - name: snap-req-other-base
   file: %s
 - name: other-base
   file: %s
`, snapdFname, core18Fname, kernelFname, gadgetFname, snapFname, baseFname))
	err := ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()
	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)

	checkOrder(c, tsAll, "snapd", "core18", "pc-kernel", "pc", "other-base", "snap-req-other-base")
}

func (s *FirstBootTestSuite) TestFirstbootGadgetBaseModelBaseMismatch(c *C) {
	s.WriteAssertions("developer.account", s.devAcct)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", map[string]interface{}{"base": "core18"})
	s.WriteAssertions("model.asserts", assertsChain...)

	core18Fname, snapdFname, kernelFname, _ := s.makeCore18Snaps(c, nil)
	// take the gadget without "base: core18"
	_, _, gadgetFname := s.makeCoreSnaps(c, "")

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: snapd
   file: %s
 - name: core18
   file: %s
 - name: pc-kernel
   file: %s
 - name: pc
   file: %s
`, snapdFname, core18Fname, kernelFname, gadgetFname))
	err := ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()

	_, err = devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, ErrorMatches, `cannot use gadget snap because its base "core" is different from model base "core18"`)
}

func (s *FirstBootTestSuite) TestPopulateFromSeedWrongContentProviderOrder(c *C) {
	loader := boottest.NewMockBootloader("mock", c.MkDir())
	bootloader.Force(loader)
	defer bootloader.Force(nil)
	boottest.SetBootKernel("pc-kernel_1.snap", loader)
	boottest.SetBootBase("core_1.snap", loader)

	coreFname, kernelFname, gadgetFname := s.makeCoreSnaps(c, "")

	// a snap that uses content providers
	snapYaml := `name: gnome-calculator
version: 1.0
plugs:
 gtk-3-themes:
  interface: content
  default-provider: gtk-common-themes
  target: $SNAP/data-dir/themes
`
	calcFname, calcDecl, calcRev := s.MakeAssertedSnap(c, snapYaml, nil, snap.R(128), "developerid")
	s.WriteAssertions("calc.asserts", s.devAcct, calcRev, calcDecl)

	// put a 2nd firstboot snap into the SnapBlobDir
	snapYaml = `name: gtk-common-themes
version: 1.0
slots:
 gtk-3-themes:
  interface: content
  source:
   read:
    - $SNAP/share/themes/Adawaita
`
	themesFname, themesDecl, themesRev := s.MakeAssertedSnap(c, snapYaml, nil, snap.R(65), "developerid")
	s.WriteAssertions("themes.asserts", s.devAcct, themesDecl, themesRev)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", nil)
	s.WriteAssertions("model.asserts", assertsChain...)

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: core
   file: %s
 - name: pc-kernel
   file: %s
 - name: pc
   file: %s
 - name: gnome-calculator
   file: %s
 - name: gtk-common-themes
   file: %s
`, coreFname, kernelFname, gadgetFname, calcFname, themesFname))
	err := ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()

	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)
	// use the expected kind otherwise settle with start another one
	chg := st.NewChange("seed", "run the populate from seed changes")
	for _, ts := range tsAll {
		chg.AddAll(ts)
	}
	c.Assert(st.Changes(), HasLen, 1)

	// avoid device reg
	chg1 := st.NewChange("become-operational", "init device")
	chg1.SetStatus(state.DoingStatus)

	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()

	c.Assert(chg.Err(), IsNil)
	c.Assert(err, IsNil)

	// verify the result
	var conns map[string]interface{}
	err = st.Get("conns", &conns)
	c.Assert(err, IsNil)
	c.Check(conns, HasLen, 1)
	conn, hasConn := conns["gnome-calculator:gtk-3-themes gtk-common-themes:gtk-3-themes"]
	c.Check(hasConn, Equals, true)
	c.Check(conn.(map[string]interface{})["auto"], Equals, true)
	c.Check(conn.(map[string]interface{})["interface"], Equals, "content")
}

func (s *FirstBootTestSuite) TestPopulateFromSeedMissingBase(c *C) {
	s.WriteAssertions("developer.account", s.devAcct)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model", nil)
	s.WriteAssertions("model.asserts", assertsChain...)

	coreFname, kernelFname, gadgetFname := s.makeCoreSnaps(c, "")

	// TODO: this test doesn't particularly need to use a local snap
	// local snap with unknown base
	snapYaml = `name: local
base: foo
version: 1.0`
	mockSnapFile := snaptest.MakeTestSnapWithFiles(c, snapYaml, nil)
	localFname := filepath.Base(mockSnapFile)
	targetSnapFile2 := filepath.Join(dirs.SnapSeedDir, "snaps", localFname)
	c.Assert(os.Rename(mockSnapFile, targetSnapFile2), IsNil)

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: core
   file: %s
 - name: pc-kernel
   file: %s
 - name: pc
   file: %s
 - name: local
   unasserted: true
   file: %s
`, coreFname, kernelFname, gadgetFname, localFname))

	c.Assert(ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644), IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, ErrorMatches, `cannot use snap "local": base "foo" is missing`)
}

func (s *FirstBootTestSuite) TestPopulateFromSeedOnClassicWithSnapdOnlyHappy(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	var sysdLog [][]string
	systemctlRestorer := systemd.MockSystemctl(func(cmd ...string) ([]byte, error) {
		sysdLog = append(sysdLog, cmd)
		return []byte("ActiveState=inactive\n"), nil
	})
	defer systemctlRestorer()

	core18Fname, snapdFname, _, _ := s.makeCore18Snaps(c, &core18SnapsOpts{
		classic: true,
	})

	// put a firstboot snap into the SnapBlobDir
	snapYaml := `name: foo
version: 1.0
base: core18
`
	fooFname, fooDecl, fooRev := s.MakeAssertedSnap(c, snapYaml, nil, snap.R(128), "developerid")
	s.WriteAssertions("foo.asserts", s.devAcct, fooRev, fooDecl)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model-classic", nil)
	s.WriteAssertions("model.asserts", assertsChain...)

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: snapd
   file: %s
 - name: foo
   file: %s
 - name: core18
   file: %s
`, snapdFname, fooFname, core18Fname))
	err := ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()
	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)

	checkOrder(c, tsAll, "snapd", "core18", "foo")

	// now run the change and check the result
	// use the expected kind otherwise settle with start another one
	chg := st.NewChange("seed", "run the populate from seed changes")
	for _, ts := range tsAll {
		chg.AddAll(ts)
	}
	c.Assert(st.Changes(), HasLen, 1)

	c.Assert(chg.Err(), IsNil)

	// avoid device reg
	chg1 := st.NewChange("become-operational", "init device")
	chg1.SetStatus(state.DoingStatus)

	// run change until it wants to restart
	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(err, IsNil)

	// at this point the system is "restarting", pretend the restart has
	// happened
	c.Assert(chg.Status(), Equals, state.DoingStatus)
	state.MockRestarting(st, state.RestartUnset)
	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(err, IsNil)
	c.Assert(chg.Status(), Equals, state.DoneStatus)

	// verify
	r, err := os.Open(dirs.SnapStateFile)
	c.Assert(err, IsNil)
	state, err := state.ReadState(nil, r)
	c.Assert(err, IsNil)

	state.Lock()
	defer state.Unlock()
	// check snapd, core18, kernel, gadget
	_, err = snapstate.CurrentInfo(state, "snapd")
	c.Check(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "core18")
	c.Check(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "foo")
	c.Check(err, IsNil)

	// and ensure state is now considered seeded
	var seeded bool
	err = state.Get("seeded", &seeded)
	c.Assert(err, IsNil)
	c.Check(seeded, Equals, true)

	// check we set seed-time
	var seedTime time.Time
	err = state.Get("seed-time", &seedTime)
	c.Assert(err, IsNil)
	c.Check(seedTime.IsZero(), Equals, false)
}

func (s *FirstBootTestSuite) TestPopulateFromSeedOnClassicWithSnapdOnlyAndGadgetHappy(c *C) {
	restore := release.MockOnClassic(true)
	defer restore()

	var sysdLog [][]string
	systemctlRestorer := systemd.MockSystemctl(func(cmd ...string) ([]byte, error) {
		sysdLog = append(sysdLog, cmd)
		return []byte("ActiveState=inactive\n"), nil
	})
	defer systemctlRestorer()

	core18Fname, snapdFname, _, gadgetFname := s.makeCore18Snaps(c, &core18SnapsOpts{
		classic: true,
		gadget:  true,
	})

	// put a firstboot snap into the SnapBlobDir
	snapYaml := `name: foo
version: 1.0
base: core18
`
	fooFname, fooDecl, fooRev := s.MakeAssertedSnap(c, snapYaml, nil, snap.R(128), "developerid")

	s.WriteAssertions("foo.asserts", s.devAcct, fooRev, fooDecl)

	// add a model assertion and its chain
	assertsChain := s.makeModelAssertionChain(c, "my-model-classic", map[string]interface{}{"gadget": "pc"})
	s.WriteAssertions("model.asserts", assertsChain...)

	// create a seed.yaml
	content := []byte(fmt.Sprintf(`
snaps:
 - name: snapd
   file: %s
 - name: foo
   file: %s
 - name: core18
   file: %s
 - name: pc
   file: %s
`, snapdFname, fooFname, core18Fname, gadgetFname))
	err := ioutil.WriteFile(filepath.Join(dirs.SnapSeedDir, "seed.yaml"), content, 0644)
	c.Assert(err, IsNil)

	// run the firstboot stuff
	st := s.overlord.State()
	st.Lock()
	defer st.Unlock()
	tsAll, err := devicestate.PopulateStateFromSeedImpl(st, s.perfTimings)
	c.Assert(err, IsNil)

	checkOrder(c, tsAll, "snapd", "core18", "pc", "foo")

	// now run the change and check the result
	// use the expected kind otherwise settle with start another one
	chg := st.NewChange("seed", "run the populate from seed changes")
	for _, ts := range tsAll {
		chg.AddAll(ts)
	}
	c.Assert(st.Changes(), HasLen, 1)

	c.Assert(chg.Err(), IsNil)

	// avoid device reg
	chg1 := st.NewChange("become-operational", "init device")
	chg1.SetStatus(state.DoingStatus)

	// run change until it wants to restart
	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(err, IsNil)

	// at this point the system is "restarting", pretend the restart has
	// happened
	c.Assert(chg.Status(), Equals, state.DoingStatus)
	state.MockRestarting(st, state.RestartUnset)
	st.Unlock()
	err = s.overlord.Settle(settleTimeout)
	st.Lock()
	c.Assert(err, IsNil)
	c.Assert(chg.Status(), Equals, state.DoneStatus, Commentf("%s", chg.Err()))

	// verify
	r, err := os.Open(dirs.SnapStateFile)
	c.Assert(err, IsNil)
	state, err := state.ReadState(nil, r)
	c.Assert(err, IsNil)

	state.Lock()
	defer state.Unlock()
	// check snapd, core18, kernel, gadget
	_, err = snapstate.CurrentInfo(state, "snapd")
	c.Check(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "core18")
	c.Check(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "pc")
	c.Check(err, IsNil)
	_, err = snapstate.CurrentInfo(state, "foo")
	c.Check(err, IsNil)

	// and ensure state is now considered seeded
	var seeded bool
	err = state.Get("seeded", &seeded)
	c.Assert(err, IsNil)
	c.Check(seeded, Equals, true)

	// check we set seed-time
	var seedTime time.Time
	err = state.Get("seed-time", &seedTime)
	c.Assert(err, IsNil)
	c.Check(seedTime.IsZero(), Equals, false)
}
