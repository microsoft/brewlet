package node

import "testing"

func TestReservedLinuxIdentityCannotEnterInitialOrExecProcess(t *testing.T) {
	for _, change := range []func(*uint32, *uint32, *[]uint32){
		func(uid, gid *uint32, groups *[]uint32) { *uid = ^uint32(0) },
		func(uid, gid *uint32, groups *[]uint32) { *gid = ^uint32(0) },
		func(uid, gid *uint32, groups *[]uint32) { *groups = []uint32{^uint32(0)} },
	} {
		p := testProcess()
		change(&p.User.UID, &p.User.GID, &p.User.AdditionalGids)
		if reason(MergeProcess(&p, testMetadata())) != "RootNotSupported" {
			t.Fatal("reserved Linux identity accepted")
		}
		initial := testProcess()
		if reason(RewriteExec(&p, InitialState{Process: initial})) != "RootNotSupported" {
			t.Fatal("reserved exec identity accepted")
		}
	}
}
