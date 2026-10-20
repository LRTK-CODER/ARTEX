package db

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRegisterTaskAssetScopesCreatesAssetsAndPersistsTextScope(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { d.Close() })

	task, err := d.CreateTask("manual scope registration", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	suffix := time.Now().UnixNano()
	domain := fmt.Sprintf("manual-scope-%d.example.test", suffix)
	ip := fmt.Sprintf("203.0.%d.%d", (suffix/250)%250, suffix%250+1)
	inputs := []ScopeInput{
		{Kind: "domain", Value: domain},
		{Kind: "ip", Value: ip},
		{Kind: "cidr", Value: "198.51.100.0/24"},
		{Kind: "icp", Value: " 京 ICP 备 12345678 号-1 "},
		{Kind: "keyword", Value: " Acme   Security "},
	}

	first, err := d.Assets().RegisterTaskAssetScopes(task.ID, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if first.Requested != 5 || first.AssetsLinked != 2 || first.AssetsExisting != 0 || first.ScopesAdded != 5 || first.ScopesExisting != 0 {
		t.Fatalf("unexpected first mutation: %+v", first)
	}
	assets, err := d.Assets().QueryByTask(task.ID, "", 10, 0)
	if err != nil || len(assets) != 2 {
		t.Fatalf("task assets=%+v err=%v", assets, err)
	}
	assetIDs := make([]int64, 0, len(assets))
	for _, asset := range assets {
		assetIDs = append(assetIDs, asset.ID)
		if asset.TaskSource != "manual" || asset.TaskSourceSummary != manualTaskScopeSummary {
			t.Fatalf("unexpected task asset provenance: %+v", asset)
		}
	}
	t.Cleanup(func() { _, _ = d.Assets().DeleteByIDs(assetIDs) })
	scopes, err := d.Assets().ListTaskScope(task.ID)
	if err != nil || len(scopes) != 5 {
		t.Fatalf("task scope=%+v err=%v", scopes, err)
	}
	values := map[string]string{}
	for _, scope := range scopes {
		values[scope.Kind] = scope.Value
	}
	if values["icp"] != NormalizeICP(inputs[3].Value) || values["keyword"] != normalizeKeyword(inputs[4].Value) {
		t.Fatalf("text scope not normalized: %+v", values)
	}

	second, err := d.Assets().RegisterTaskAssetScopes(task.ID, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if second.AssetsExisting != 2 || second.AssetsLinked != 0 || second.ScopesExisting != 5 || second.ScopesAdded != 0 {
		t.Fatalf("unexpected idempotent mutation: %+v", second)
	}

	rollbackDomain := fmt.Sprintf("rollback-%d.example.test", suffix)
	_, err = d.Assets().RegisterTaskAssetScopes(task.ID, []ScopeInput{
		{Kind: "domain", Value: rollbackDomain},
		{Kind: "cidr", Value: "10.0.0.0/8"},
	})
	if !errors.Is(err, ErrTaskAssetInvalid) {
		t.Fatalf("invalid CIDR error=%v", err)
	}
	var rollbackAssets int
	if err := d.QueryRow(`SELECT count(*) FROM assets WHERE type='root_domain' AND domain=$1`, rollbackDomain).Scan(&rollbackAssets); err != nil {
		t.Fatal(err)
	}
	if rollbackAssets != 0 {
		t.Fatalf("invalid request created %d assets", rollbackAssets)
	}
}

func TestTaskAssetAttachDetachPreservesGlobalAssetAndAnchors(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { d.Close() })

	task, err := d.CreateTask("asset editing", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	assetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("manual-%d.example.test", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Assets().DeleteByIDs([]int64{assetID}) })

	mutation, err := d.Assets().AttachAssetsToTask(task.ID, []int64{assetID, assetID}, "허가된 자산 목록 3번 항목")
	if err != nil {
		t.Fatal(err)
	}
	if mutation.Requested != 1 || mutation.Attached != 1 || mutation.Existing != 0 {
		t.Fatalf("unexpected first mutation: %+v", mutation)
	}
	assets, err := d.Assets().QueryByTask(task.ID, "", 10, 0)
	if err != nil || len(assets) != 1 {
		t.Fatalf("task assets=%+v err=%v", assets, err)
	}
	if assets[0].TaskSource != "manual" || assets[0].TaskSourceSummary != "허가된 자산 목록 3번 항목" {
		t.Fatalf("unexpected provenance: %+v", assets[0])
	}

	intentID, err := d.Exploration(task.ExplorationID).AddIntent(map[string]any{"summary": "test asset"}, 5, []int64{assetID}, "human")
	if err != nil {
		t.Fatal(err)
	}
	detached, err := d.Assets().DetachAssetFromTask(task.ID, assetID)
	if err != nil || !detached {
		t.Fatalf("detach=%v err=%v", detached, err)
	}
	if assets, err := d.Assets().QueryByTask(task.ID, "", 10, 0); err != nil || len(assets) != 0 {
		t.Fatalf("detached task assets=%+v err=%v", assets, err)
	}
	var global, anchors, links int
	_ = d.QueryRow(`SELECT count(*) FROM assets WHERE id=$1`, assetID).Scan(&global)
	_ = d.QueryRow(`SELECT count(*) FROM exploration_anchors WHERE node_id=$1 AND asset_id=$2`, intentID, assetID).Scan(&anchors)
	_ = d.QueryRow(`SELECT count(*) FROM task_asset_links WHERE task_id=$1 AND asset_id=$2`, task.ID, assetID).Scan(&links)
	if global != 1 || anchors != 1 || links != 0 {
		t.Fatalf("global=%d anchors=%d links=%d", global, anchors, links)
	}
}

func TestIntentAssetsIncludesDirectSourceProvenance(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { d.Close() })

	source, err := d.CreateTask("source assets", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	current, err := d.CreateTaskWithOptions("current assets", "goal", TaskCreateOptions{SourceTaskIDs: []int64{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(current.ID); _ = d.DeleteTask(source.ID) })

	assetID, err := d.Assets().UpsertHTTPService(UpsertHTTPServiceReq{
		URL: fmt.Sprintf("https://intent-%d.example.test", time.Now().UnixNano()), TaskID: source.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Assets().DeleteByIDs([]int64{assetID}) })
	intentID, err := d.Exploration(source.ExplorationID).AddIntent(map[string]any{"summary": "source worker"}, 5, []int64{assetID}, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Exploration(source.ExplorationID).SetNodeState(intentID, "done"); err != nil {
		t.Fatal(err)
	}
	nodeID := intentID
	if err := d.Assets().SetTaskAssetSource(source.ID, assetID, "agent", "워커가 insert_assets로 등록", &nodeID); err != nil {
		t.Fatal(err)
	}

	assets, err := d.Assets().IntentAssets(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].IntentID != intentID || assets[0].SourceTaskID != source.ID || !assets[0].Inherited {
		t.Fatalf("unexpected intent assets: %+v", assets)
	}
	if assets[0].Source != "agent" || assets[0].SourceSummary == "" || assets[0].SourceNodeID == nil || *assets[0].SourceNodeID != intentID {
		t.Fatalf("unexpected intent provenance: %+v", assets[0])
	}
}

// 손으로 넣은 빈 값 ICP 범위 행이 ICP 없는 자산 전부와 맞으면 작업 범위가 공유 DB 전체로
// 넓어진다(#192). 작업 범위 쿼리 네 곳 모두 그런 행을 무시해야 한다.
func TestEmptyICPTaskScopeMatchesNoAsset(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { d.Close() })

	task, err := d.CreateTask("empty icp scope", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	domain := fmt.Sprintf("empty-icp-%d.example.test", time.Now().UnixNano())
	assetID, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: domain})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Assets().DeleteByIDs([]int64{assetID}) })
	// 코드 경로는 빈 ICP를 거부하므로 SQL로 직접 넣는다.
	if _, err := d.Exec(`INSERT INTO task_scope(task_id, kind, value, source) VALUES ($1, 'icp', '', 'manual')`, task.ID); err != nil {
		t.Fatal(err)
	}

	t.Run("scope target", func(t *testing.T) {
		got, err := d.Assets().GetByIDsInScope(task.ID, []int64{assetID})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("asset without ICP is in scope: %+v", got[0])
		}
	})
	t.Run("coverage", func(t *testing.T) {
		cov, err := d.Assets().TaskCoverage(task.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if cov.Denominator != 0 {
			t.Fatalf("coverage denominator=%d want 0", cov.Denominator)
		}
	})
	t.Run("coverage with sources", func(t *testing.T) {
		cov, err := d.Assets().TaskCoverageWithSources(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cov.Denominator != 0 {
			t.Fatalf("coverage denominator=%d want 0", cov.Denominator)
		}
	})
	t.Run("hosts", func(t *testing.T) {
		hosts, err := d.Assets().HostsByTaskWithSources(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, host := range hosts {
			if host == domain {
				t.Fatalf("hosts include asset without ICP: %s", domain)
			}
		}
	})
}
