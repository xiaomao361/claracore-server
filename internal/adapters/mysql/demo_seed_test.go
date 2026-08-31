package mysql

import (
	"testing"

	"claracore-server/internal/domain/governance"
)

func TestNewDemoCredentialsAreCompleteAndUnique(t *testing.T) {
	credentials, err := NewDemoCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != len(demoProfiles) {
		t.Fatalf("凭据数 = %d, want %d", len(credentials), len(demoProfiles))
	}
	passwords := map[string]bool{}
	tokens := map[string]bool{}
	for i, credential := range credentials {
		if credential.Username != demoProfiles[i].Username || credential.AgentSlug != demoProfiles[i].AgentSlug {
			t.Fatalf("第 %d 组身份与画像不一致: %+v", i+1, credential)
		}
		if len(credential.Password) < 20 || passwords[credential.Password] {
			t.Fatalf("第 %d 组密码强度或唯一性异常", i+1)
		}
		passwords[credential.Password] = true
		if _, err := governance.ParseToken(credential.Token); err != nil || tokens[credential.Token] {
			t.Fatalf("第 %d 组 token 形状或唯一性异常: %v", i+1, err)
		}
		tokens[credential.Token] = true
	}
}
