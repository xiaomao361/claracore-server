package governance

import (
	"errors"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

const (
	userMe    = platform.ID("01USERMEAAAAAAAAAAAAAAAAAA")
	userOther = platform.ID("01USEROTHERAAAAAAAAAAAAAAA")
	tenant1   = platform.ID("01TENANT1AAAAAAAAAAAAAAAAA")

	spTeam         = platform.ID("01SPACETEAMAAAAAAAAAAAAAAA")
	spMyPersonal   = platform.ID("01SPACEMYPERSONALAAAAAAAAA")
	spTheirPersnl  = platform.ID("01SPACETHEIRPERSONALAAAAAA")
	spMyProject    = platform.ID("01SPACEMYPROJECTAAAAAAAAAA")
	spSharedProj   = platform.ID("01SPACESHAREDPROJAAAAAAAAA")
	spStrangerProj = platform.ID("01SPACESTRANGERPROJAAAAAAA")
)

func space(id platform.ID, typ string, owner *platform.ID) Space {
	return Space{ID: id, TenantID: tenant1, Type: typ, Name: string(id), OwnerUserID: owner}
}

func owner(id platform.ID) *platform.ID { return &id }

func perm(spaceID platform.ID, read, write bool) *Permission {
	return &Permission{SpaceID: spaceID, UserID: userMe, CanRead: read, CanWrite: write}
}

// 一份覆盖全部规则分支的成员关系，多数用例共用。
func fullMemberships() []SpaceMembership {
	return []SpaceMembership{
		{Space: space(spTeam, SpaceTeam, nil)},                         // 团队空间，无显式授权
		{Space: space(spMyPersonal, SpacePersonal, owner(userMe))},     // 自己的个人空间
		{Space: space(spTheirPersnl, SpacePersonal, owner(userOther))}, // 别人的个人空间（异常数据）
		{Space: space(spMyProject, SpaceProject, owner(userMe))},       // 自己建的项目空间
		{Space: space(spSharedProj, SpaceProject, owner(userOther)), Permission: perm(spSharedProj, true, true)},
		{Space: space(spStrangerProj, SpaceProject, owner(userOther))}, // 别人的项目空间，未授权
	}
}

func TestResolveScopeRules(t *testing.T) {
	scope := ResolveScope(userMe, fullMemberships())

	t.Run("团队空间隐含可读但不隐含可写", func(t *testing.T) {
		if !scope.CanRead(spTeam) {
			t.Error("团队空间应当全员可读（02-data-model.md §2.2）")
		}
		if scope.CanWrite(spTeam) {
			t.Error("团队空间的写权限必须显式授予，不能隐含")
		}
	})

	t.Run("自己的个人空间可读可写", func(t *testing.T) {
		if !scope.CanRead(spMyPersonal) || !scope.CanWrite(spMyPersonal) {
			t.Error("个人空间对 owner 应当可读可写")
		}
	})

	t.Run("别人的个人空间完全不可见", func(t *testing.T) {
		if scope.CanRead(spTheirPersnl) || scope.CanWrite(spTheirPersnl) {
			t.Error("个人空间的封闭性是设计约束，任何情况下不对他人开放")
		}
		if _, ok := scope.Space(spTheirPersnl); ok {
			t.Error("别人的个人空间不该出现在 scope 里")
		}
	})

	t.Run("自己建的项目空间可读可写", func(t *testing.T) {
		if !scope.CanRead(spMyProject) || !scope.CanWrite(spMyProject) {
			t.Error("创建者不该被自己建的空间锁在门外")
		}
	})

	t.Run("被显式授权的项目空间按授权行", func(t *testing.T) {
		if !scope.CanRead(spSharedProj) || !scope.CanWrite(spSharedProj) {
			t.Error("显式授权的读写位应当生效")
		}
	})

	t.Run("未授权的项目空间不可见", func(t *testing.T) {
		if scope.CanRead(spStrangerProj) || scope.CanWrite(spStrangerProj) {
			t.Error("项目空间是显式加入（2026-08-06 拍板），没有授权行就没有权限")
		}
	})
}

// 团队空间的写权限只能来自显式授权行——这条单独测，
// 因为「全员可读」很容易被误实现成「全员可读写」。
func TestTeamSpaceWriteRequiresExplicitGrant(t *testing.T) {
	scope := ResolveScope(userMe, []SpaceMembership{
		{Space: space(spTeam, SpaceTeam, nil), Permission: perm(spTeam, true, true)},
	})
	if !scope.CanWrite(spTeam) {
		t.Error("显式授权后团队空间应当可写")
	}
}

func TestUnknownSpaceTypeGetsNoAccess(t *testing.T) {
	scope := ResolveScope(userMe, []SpaceMembership{
		{Space: space("01SPACEWEIRDAAAAAAAAAAAAAA", "shared", owner(userMe)), Permission: perm("01SPACEWEIRDAAAAAAAAAAAAAA", true, true)},
	})
	if len(scope.All()) != 0 {
		t.Error("未知空间类型必须无权限——默认放行会让一个拼写错误变成越权")
	}
}

func TestScopeOrderIsStable(t *testing.T) {
	want := []platform.ID{spTeam, spMyProject, spSharedProj, spMyPersonal}

	// 输入顺序不同，输出顺序必须一致
	shuffled := fullMemberships()
	shuffled[0], shuffled[4] = shuffled[4], shuffled[0]

	for _, in := range [][]SpaceMembership{fullMemberships(), shuffled} {
		got := ResolveScope(userMe, in).All()
		if len(got) != len(want) {
			t.Fatalf("空间数不符: got %d, want %d", len(got), len(want))
		}
		for i := range got {
			if got[i].Space.ID != want[i] {
				t.Errorf("第 %d 个空间应为 %s，实际 %s（顺序抖动会让 agent 的选择跟着抖）", i, want[i], got[i].Space.ID)
			}
		}
	}
}

func TestResolveWriteTarget(t *testing.T) {
	scope := ResolveScope(userMe, fullMemberships())

	t.Run("未指定时默认个人空间", func(t *testing.T) {
		got, err := scope.ResolveWriteTarget("")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != spMyPersonal {
			t.Errorf("默认写入目标应为个人空间，实际 %s", got.ID)
		}
	})

	t.Run("指定可写空间时通过", func(t *testing.T) {
		got, err := scope.ResolveWriteTarget(spMyProject)
		if err != nil || got.ID != spMyProject {
			t.Errorf("应当允许写入自己的项目空间: %v", err)
		}
	})

	t.Run("指定只读空间时拒绝", func(t *testing.T) {
		_, err := scope.ResolveWriteTarget(spTeam)
		if !errors.Is(err, ErrSpaceForbidden) {
			t.Errorf("写入只读的团队空间应当被拒绝，得到 %v", err)
		}
	})

	t.Run("不存在与无权限返回同一个错误", func(t *testing.T) {
		_, errNoSuch := scope.ResolveWriteTarget("01SPACENOSUCHAAAAAAAAAAAAA")
		_, errForbidden := scope.ResolveWriteTarget(spStrangerProj)
		if !errors.Is(errNoSuch, ErrSpaceForbidden) || !errors.Is(errForbidden, ErrSpaceForbidden) {
			t.Error("区分「不存在」与「无权限」等于泄露空间是否存在")
		}
	})

	t.Run("没有个人空间时报明确错误", func(t *testing.T) {
		empty := ResolveScope(userMe, []SpaceMembership{{Space: space(spTeam, SpaceTeam, nil)}})
		_, err := empty.ResolveWriteTarget("")
		if !errors.Is(err, ErrNoPersonalSpace) {
			t.Errorf("应当报 ErrNoPersonalSpace，得到 %v", err)
		}
	})
}

func TestResolveReadTargets(t *testing.T) {
	scope := ResolveScope(userMe, fullMemberships())

	t.Run("未指定时覆盖全部可读空间", func(t *testing.T) {
		got, err := scope.ResolveReadTargets(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 4 {
			t.Errorf("应覆盖 4 个可读空间，实际 %d 个: %v", len(got), got)
		}
	})

	t.Run("指定不可读空间时报错而非静默跳过", func(t *testing.T) {
		_, err := scope.ResolveReadTargets([]platform.ID{spTeam, spStrangerProj})
		if !errors.Is(err, ErrSpaceForbidden) {
			t.Errorf("静默过滤会让 agent 以为搜过了那个空间并得到「没有结果」，得到 %v", err)
		}
	})

	t.Run("重复的空间被去重", func(t *testing.T) {
		got, err := scope.ResolveReadTargets([]platform.ID{spTeam, spTeam, spMyPersonal})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("应去重为 2 个，实际 %v", got)
		}
	})
}

func TestTokenValidate(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	cases := []struct {
		name  string
		token Token
		want  error
	}{
		{"无过期时间且未吊销", Token{}, nil},
		{"未到期", Token{ExpiresAt: &future}, nil},
		{"已过期", Token{ExpiresAt: &past}, ErrTokenExpired},
		{"恰好在过期时刻", Token{ExpiresAt: &now}, ErrTokenExpired},
		{"已吊销", Token{RevokedAt: &past}, ErrTokenRevoked},
		{"已吊销优先于未过期", Token{RevokedAt: &past, ExpiresAt: &future}, ErrTokenRevoked},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.token.Validate(now); !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}
