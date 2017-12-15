/*
Copyright 2017 the Heptio Ark Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"io"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/clock"
	core "k8s.io/client-go/testing"

	testlogger "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/heptio/ark/pkg/apis/ark/v1"
	"github.com/heptio/ark/pkg/backup"
	"github.com/heptio/ark/pkg/cloudprovider"
	"github.com/heptio/ark/pkg/generated/clientset/versioned/fake"
	"github.com/heptio/ark/pkg/generated/clientset/versioned/scheme"
	informers "github.com/heptio/ark/pkg/generated/informers/externalversions"
	"github.com/heptio/ark/pkg/restore"
	. "github.com/heptio/ark/pkg/util/test"
)

type fakeBackupper struct {
	mock.Mock
}

func (b *fakeBackupper) Backup(backup *v1.Backup, data, log io.Writer, actions []backup.ItemAction) error {
	args := b.Called(backup, data, log, actions)
	return args.Error(0)
}

func TestProcessBackup(t *testing.T) {
	tests := []struct {
		name             string
		key              string
		expectError      bool
		expectedIncludes []string
		expectedExcludes []string
		backup           *TestBackup
		expectBackup     bool
		allowSnapshots   bool
	}{
		{
			name:        "bad key",
			key:         "bad/key/here",
			expectError: true,
		},
		{
			name:        "lister failed",
			key:         "heptio-ark/backup1",
			expectError: true,
		},
		{
			name:         "do not process phase FailedValidation",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseFailedValidation),
			expectBackup: false,
		},
		{
			name:         "do not process phase InProgress",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseInProgress),
			expectBackup: false,
		},
		{
			name:         "do not process phase Completed",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseCompleted),
			expectBackup: false,
		},
		{
			name:         "do not process phase Failed",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseFailed),
			expectBackup: false,
		},
		{
			name:         "do not process phase other",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase("arg"),
			expectBackup: false,
		},
		{
			name:         "invalid included/excluded resources fails validation",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseNew).WithIncludedResources("foo").WithExcludedResources("foo"),
			expectBackup: false,
		},
		{
			name:         "invalid included/excluded namespaces fails validation",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseNew).WithIncludedNamespaces("foo").WithExcludedNamespaces("foo"),
			expectBackup: false,
		},
		{
			name:             "make sure specified included and excluded resources are honored",
			key:              "heptio-ark/backup1",
			backup:           NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseNew).WithIncludedResources("i", "j").WithExcludedResources("k", "l"),
			expectedIncludes: []string{"i", "j"},
			expectedExcludes: []string{"k", "l"},
			expectBackup:     true,
		},
		{
			name:         "if includednamespaces are specified, don't default to *",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseNew).WithIncludedNamespaces("ns-1"),
			expectBackup: true,
		},
		{
			name:         "ttl",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseNew).WithTTL(10 * time.Minute),
			expectBackup: true,
		},
		{
			name:         "backup with SnapshotVolumes when allowSnapshots=false fails validation",
			key:          "heptio-ark/backup1",
			backup:       NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseNew).WithSnapshotVolumes(true),
			expectBackup: false,
		},
		{
			name:           "backup with SnapshotVolumes when allowSnapshots=true gets executed",
			key:            "heptio-ark/backup1",
			backup:         NewTestBackup().WithName("backup1").WithPhase(v1.BackupPhaseNew).WithSnapshotVolumes(true),
			allowSnapshots: true,
			expectBackup:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				client          = fake.NewSimpleClientset()
				backupper       = &fakeBackupper{}
				cloudBackups    = &BackupService{}
				sharedInformers = informers.NewSharedInformerFactory(client, 0)
				logger, _       = testlogger.NewNullLogger()
				pluginManager   = &MockManager{}
			)

			c := NewBackupController(
				sharedInformers.Ark().V1().Backups(),
				client.ArkV1(),
				backupper,
				cloudBackups,
				"bucket",
				test.allowSnapshots,
				logger,
				pluginManager,
			).(*backupController)
			c.clock = clock.NewFakeClock(time.Now())

			var expiration time.Time

			if test.backup != nil {
				// add directly to the informer's store so the lister can function and so we don't have to
				// start the shared informers.
				sharedInformers.Ark().V1().Backups().Informer().GetStore().Add(test.backup.Backup)

				if test.backup.Spec.TTL.Duration > 0 {
					expiration = c.clock.Now().Add(test.backup.Spec.TTL.Duration)
				}

				// set up a Backup object to represent what we expect to be passed to backupper.Backup()
				copy, err := scheme.Scheme.Copy(test.backup.Backup)
				assert.NoError(t, err, "copy error")
				backup := copy.(*v1.Backup)
				backup.Spec.IncludedResources = test.expectedIncludes
				backup.Spec.ExcludedResources = test.expectedExcludes
				backup.Spec.IncludedNamespaces = test.backup.Spec.IncludedNamespaces
				backup.Spec.SnapshotVolumes = test.backup.Spec.SnapshotVolumes
				backup.Status.Phase = v1.BackupPhaseInProgress
				backup.Status.Expiration.Time = expiration
				backup.Status.Version = 1
				backupper.On("Backup", backup, mock.Anything, mock.Anything, mock.Anything).Return(nil)

				cloudBackups.On("UploadBackup", "bucket", backup.Name, mock.Anything, mock.Anything, mock.Anything).Return(nil)

				pluginManager.On("GetBackupItemActions", backup.Name).Return(nil, nil)
				pluginManager.On("CloseBackupItemActions", backup.Name).Return(nil)
			}

			// this is necessary so the Update() call returns the appropriate object
			client.PrependReactor("update", "backups", func(action core.Action) (bool, runtime.Object, error) {
				obj := action.(core.UpdateAction).GetObject()
				// need to deep copy so we can test the backup state for each call to update
				copy, err := scheme.Scheme.DeepCopy(obj)
				if err != nil {
					return false, nil, err
				}
				ret := copy.(runtime.Object)
				return true, ret, nil
			})

			// method under test
			err := c.processBackup(test.key)

			if test.expectError {
				require.Error(t, err, "processBackup should error")
				return
			}
			require.NoError(t, err, "processBackup unexpected error: %v", err)

			if !test.expectBackup {
				assert.Empty(t, backupper.Calls)
				assert.Empty(t, cloudBackups.Calls)
				return
			}

			expectedActions := []core.Action{
				core.NewUpdateAction(
					v1.SchemeGroupVersion.WithResource("backups"),
					v1.DefaultNamespace,
					NewTestBackup().
						WithName(test.backup.Name).
						WithPhase(v1.BackupPhaseInProgress).
						WithIncludedResources(test.expectedIncludes...).
						WithExcludedResources(test.expectedExcludes...).
						WithIncludedNamespaces(test.backup.Spec.IncludedNamespaces...).
						WithTTL(test.backup.Spec.TTL.Duration).
						WithSnapshotVolumesPointer(test.backup.Spec.SnapshotVolumes).
						WithExpiration(expiration).
						WithVersion(1).
						Backup,
				),

				core.NewUpdateAction(
					v1.SchemeGroupVersion.WithResource("backups"),
					v1.DefaultNamespace,
					NewTestBackup().
						WithName(test.backup.Name).
						WithPhase(v1.BackupPhaseCompleted).
						WithIncludedResources(test.expectedIncludes...).
						WithExcludedResources(test.expectedExcludes...).
						WithIncludedNamespaces(test.backup.Spec.IncludedNamespaces...).
						WithTTL(test.backup.Spec.TTL.Duration).
						WithSnapshotVolumesPointer(test.backup.Spec.SnapshotVolumes).
						WithExpiration(expiration).
						WithVersion(1).
						Backup,
				),
			}

			assert.Equal(t, expectedActions, client.Actions())
		})
	}
}

// MockManager is an autogenerated mock type for the Manager type
type MockManager struct {
	mock.Mock
}

// CloseBackupItemActions provides a mock function with given fields: backupName
func (_m *MockManager) CloseBackupItemActions(backupName string) error {
	ret := _m.Called(backupName)

	var r0 error
	if rf, ok := ret.Get(0).(func(string) error); ok {
		r0 = rf(backupName)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// GetBackupItemActions provides a mock function with given fields: backupName, logger, level
func (_m *MockManager) GetBackupItemActions(backupName string) ([]backup.ItemAction, error) {
	ret := _m.Called(backupName)

	var r0 []backup.ItemAction
	if rf, ok := ret.Get(0).(func(string) []backup.ItemAction); ok {
		r0 = rf(backupName)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).([]backup.ItemAction)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(backupName)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// CloseRestoreItemActions provides a mock function with given fields: restoreName
func (_m *MockManager) CloseRestoreItemActions(restoreName string) error {
	ret := _m.Called(restoreName)

	var r0 error
	if rf, ok := ret.Get(0).(func(string) error); ok {
		r0 = rf(restoreName)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// GetRestoreItemActions provides a mock function with given fields: restoreName, logger, level
func (_m *MockManager) GetRestoreItemActions(restoreName string) ([]restore.ItemAction, error) {
	ret := _m.Called(restoreName)

	var r0 []restore.ItemAction
	if rf, ok := ret.Get(0).(func(string) []restore.ItemAction); ok {
		r0 = rf(restoreName)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).([]restore.ItemAction)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(restoreName)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetBlockStore provides a mock function with given fields: name
func (_m *MockManager) GetBlockStore(name string) (cloudprovider.BlockStore, error) {
	ret := _m.Called(name)

	var r0 cloudprovider.BlockStore
	if rf, ok := ret.Get(0).(func(string) cloudprovider.BlockStore); ok {
		r0 = rf(name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(cloudprovider.BlockStore)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetObjectStore provides a mock function with given fields: name
func (_m *MockManager) GetObjectStore(name string) (cloudprovider.ObjectStore, error) {
	ret := _m.Called(name)

	var r0 cloudprovider.ObjectStore
	if rf, ok := ret.Get(0).(func(string) cloudprovider.ObjectStore); ok {
		r0 = rf(name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(cloudprovider.ObjectStore)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}
