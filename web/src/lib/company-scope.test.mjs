import { parseCompanyScopeText } from "./company-scope.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("icp나 备案이 든 글은 ICP가 아니라 키워드로 분류한다", () => {
  const parsed = parseCompanyScopeText("京ICP备12345678号-1\n沪网备案 9988\nicp 담당 부서");
  assert.deepEqual(parsed.errors, []);
  assert.deepEqual(
    parsed.rules.map((rule) => rule.kind),
    ["keyword", "keyword", "keyword"],
  );
});

test("편집 중인 예전 ICP 범위는 종류를 유지한다", () => {
  const parsed = parseCompanyScopeText("京ICP备12345678号-1", {
    preservedRules: [{ kind: "icp", value: "京ICP备12345678号-1" }],
  });
  assert.deepEqual(parsed.rules, [{ kind: "icp", value: "京ICP备12345678号-1" }]);
});
