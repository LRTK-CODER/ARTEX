"use client";

import * as React from "react";

import { PlayIcon, PlusIcon, RotateCcwIcon, SaveIcon, SearchIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Separator } from "@/components/ui/separator";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { Agent, Tool } from "@/lib/types";

// 트래픽 도구는 전역 트래픽 캡처 스위치에 묶인 호스트 도구다. 연결은 할 수 있지만
// 캡처가 켜져 있을 때만 쓸 수 있다. traffic.SeedToolMetas와 맞춰 둔다.
const TRAFFIC_TOOL_KEYS = new Set(["traffic_search", "traffic_get"]);

// paramRows flattens schema.properties into an ordered row list for editing.
// name/type/required are read-only (welded to the Go handler); description/default
// are editable. Non-scalar params (array/object) can't carry an editable default.
// parentKey is set for rows that live inside an array param's items.properties.
type ParamRow = {
  name: string;
  type: string;
  required: boolean;
  description: string;
  defaultStr: string; // "" = no default declared
  scalar: boolean;
  parentKey?: string; // set when this is a sub-field of an array param's items
};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function toRows(schema: Record<string, any>): ParamRow[] {
  const props = (schema?.properties ?? {}) as Record<string, Record<string, unknown>>;
  const required = new Set<string>((schema?.required as string[]) ?? []);
  const rows: ParamRow[] = [];
  for (const [name, p] of Object.entries(props)) {
    const type = String(p?.type ?? "");
    const hasDefault = p != null && "default" in p && (p as Record<string, unknown>).default != null;
    rows.push({
      name,
      type,
      required: required.has(name),
      description: String(p?.description ?? ""),
      defaultStr: hasDefault ? String((p as Record<string, unknown>).default) : "",
      scalar: ["string", "integer", "number", "boolean"].includes(type),
    });
    // Expand items.properties for array params so sub-fields are editable.
    if (type === "array") {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const itemProps = (p as any)?.items?.properties as Record<string, Record<string, unknown>> | undefined;
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const itemRequired = new Set<string>(((p as any)?.items?.required as string[]) ?? []);
      if (itemProps) {
        for (const [subName, subP] of Object.entries(itemProps)) {
          const subType = String((subP as Record<string, unknown>)?.type ?? "");
          const subHasDefault = subP != null && "default" in subP && subP.default != null;
          rows.push({
            name: subName,
            type: subType,
            required: itemRequired.has(subName),
            description: String(subP?.description ?? ""),
            defaultStr: subHasDefault ? String(subP.default) : "",
            scalar: ["string", "integer", "number", "boolean"].includes(subType),
            parentKey: name,
          });
        }
      }
    }
  }
  return rows;
}

// coerceDefault turns the edited default string back into a typed JSON value, or
// undefined to drop the "default" key entirely.
function coerceDefault(type: string, raw: string): unknown {
  const s = raw.trim();
  if (s === "") return undefined;
  if (type === "integer" || type === "number") {
    const n = Number(s);
    return Number.isNaN(n) ? undefined : n;
  }
  if (type === "boolean") return s === "true";
  return raw;
}

// applyRows writes edited rows back into a deep-copied schema (structure untouched).
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function applyRows(schema: Record<string, any>, rows: ParamRow[]): Record<string, any> {
  const next = structuredClone(schema ?? {});
  const props = (next.properties ?? {}) as Record<string, Record<string, unknown>>;
  for (const r of rows) {
    if (r.parentKey) {
      // Sub-field of an array param's items.properties
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const parent = props[r.parentKey] as any;
      const subP = parent?.items?.properties?.[r.name] as Record<string, unknown> | undefined;
      if (!subP) continue;
      subP.description = r.description;
      if (r.scalar) {
        const dv = coerceDefault(r.type, r.defaultStr);
        if (dv === undefined) delete subP.default;
        else subP.default = dv;
      }
    } else {
      const p = props[r.name];
      if (!p) continue;
      p.description = r.description;
      if (r.scalar) {
        const dv = coerceDefault(r.type, r.defaultStr);
        if (dv === undefined) delete p.default;
        else p.default = dv;
      }
    }
  }
  return next;
}

// ToolEditor is the full edit form for one tool, rendered inside the drawer.
function ToolEditor({
  tool,
  agents,
  captureOn,
  onSaved,
  onClose,
}: {
  tool: Tool;
  agents: Agent[];
  captureOn: boolean;
  onSaved: () => void;
  onClose: () => void;
}) {
  // 트래픽 도구는 전역 트래픽 캡처 스위치가 켜지기 전에는 연결하거나 사용할 수 없다.
  const trafficGated = TRAFFIC_TOOL_KEYS.has(tool.key) && !captureOn;
  const [description, setDescription] = React.useState(tool.description);
  const [bound, setBound] = React.useState<string[]>(tool.agents);
  const [enabled, setEnabled] = React.useState(tool.enabled);
  const [rows, setRows] = React.useState<ParamRow[]>(() => toRows(tool.schema));
  const [saving, setSaving] = React.useState(false);

  const setRow = (i: number, patch: Partial<ParamRow>) =>
    setRows((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const toggleAgent = (k: string) => setBound((b) => (b.includes(k) ? b.filter((x) => x !== k) : [...b, k]));

  async function save() {
    setSaving(true);
    try {
      await api.saveTool(tool.key, {
        description,
        schema: applyRows(tool.schema, rows),
        agents: bound,
        enabled,
      });
      toast.success(`도구 「${tool.key}」을(를) 저장했습니다`);
      onSaved();
      onClose();
    } catch (e) {
      toast.error("저장하지 못했습니다: " + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }
  async function reset() {
    try {
      await api.resetTool(tool.key);
      toast.success(`「${tool.key}」을(를) 코드 기본값으로 되돌렸습니다`);
      onSaved();
      onClose();
    } catch (e) {
      toast.error("되돌리지 못했습니다: " + (e as Error).message);
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4">
        {trafficGated && (
          <div className="border-amber-500/40 bg-amber-500/10 text-muted-foreground rounded-md border px-3 py-2 text-xs">
            이 도구는 <b>트래픽 캡처</b>가 필요합니다. 「시스템 설정」에서 트래픽 캡처를 먼저 켜야 에이전트에 연결하고
            사용할 수 있습니다.
          </div>
        )}
        {/* binding + switch */}
        <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
          <div className="grid gap-1.5">
            <Label className="text-muted-foreground text-xs">에이전트 연결(이 도구를 쓸 에이전트를 정함)</Label>
            <div className="flex flex-wrap gap-3">
              {agents.map((ag) => (
                <label key={ag.key} className="flex items-center gap-2 text-sm">
                  <Checkbox
                    checked={bound.includes(ag.key)}
                    disabled={trafficGated}
                    onCheckedChange={() => toggleAgent(ag.key)}
                  />
                  {ag.name}
                  <span className="text-muted-foreground font-mono text-xs">{ag.key}</span>
                </label>
              ))}
              {agents.length === 0 && <span className="text-muted-foreground text-xs">(에이전트 없음)</span>}
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Switch checked={enabled} onCheckedChange={setEnabled} id={`en-${tool.key}`} />
            <Label htmlFor={`en-${tool.key}`} className="text-sm">
              사용
            </Label>
          </div>
        </div>

        {/* description */}
        <div className="grid gap-1.5">
          <Label className="text-muted-foreground text-xs">도구 설명(모델에 보냄)</Label>
          <Textarea
            className="font-mono text-xs"
            rows={6}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>

        {/* params */}
        <div className="grid gap-2">
          <Label className="text-muted-foreground text-xs">
            파라미터(이름 / 유형 / 필수는 읽기 전용, 설명과 기본값은 바꿀 수 있음)
          </Label>
          {rows.length === 0 && <span className="text-muted-foreground text-xs">(파라미터 없음)</span>}
          {rows.map((r, i) => (
            <div
              key={(r.parentKey ?? "") + "." + r.name}
              className={
                r.parentKey ? "border-l-2 border-muted ml-3 pl-3 grid gap-2 py-2" : "grid gap-2 rounded-md border p-3"
              }
            >
              <div className="flex flex-wrap items-center gap-2">
                {r.parentKey && <span className="text-muted-foreground font-mono text-[10px]">↳</span>}
                <span className="font-mono text-sm">{r.name}</span>
                <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">
                  {r.type || "?"}
                </Badge>
                {r.required && (
                  <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                    필수
                  </Badge>
                )}
                {r.parentKey && <span className="text-muted-foreground text-[10px]">items 하위 필드</span>}
              </div>
              <div className="grid gap-2">
                <div className="grid gap-1">
                  <Label className="text-muted-foreground text-[11px]">설명</Label>
                  <Input
                    className="text-xs"
                    value={r.description}
                    onChange={(e) => setRow(i, { description: e.target.value })}
                  />
                </div>
                <div className="grid gap-1">
                  <Label className="text-muted-foreground text-[11px]">기본값</Label>
                  <Input
                    className="text-xs"
                    placeholder={r.scalar ? "(비우면 기본값 없음)" : "스칼라 값만 지원"}
                    disabled={!r.scalar}
                    value={r.defaultStr}
                    onChange={(e) => setRow(i, { defaultStr: e.target.value })}
                  />
                </div>
              </div>
            </div>
          ))}
        </div>
      </div>

      <Separator className="mt-4" />
      <div className="flex flex-wrap gap-2 p-4">
        <Button size="sm" onClick={save} disabled={saving}>
          <SaveIcon /> 저장
        </Button>
        <Button size="sm" variant="outline" onClick={reset}>
          <RotateCcwIcon /> 기본값으로 되돌리기
        </Button>
      </div>
    </div>
  );
}

// ToolGridCard is one clickable tile in the catalog grid.
function ToolGridCard({ tool, onClick }: { tool: Tool; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="hover:border-primary/50 hover:bg-muted/40 focus-visible:ring-ring flex flex-col gap-2 rounded-lg border p-4 text-left transition-colors focus-visible:ring-2 focus-visible:outline-none"
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm font-medium">{tool.key}</span>
        {tool.system ? (
          <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">
            시스템
          </Badge>
        ) : (
          <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
            사용자 지정·{tool.kind}
          </Badge>
        )}
        {tool.deferred && (
          <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
            deferred
          </Badge>
        )}
        {!tool.enabled && (
          <Badge variant="outline" className="text-destructive px-1.5 py-0 text-[10px]">
            사용 안 함
          </Badge>
        )}
        <Badge
          variant="secondary"
          className="ml-auto px-1.5 py-0 text-[10px] tabular-nums"
          title={`누적 호출 ${tool.calls ?? 0}회`}
        >
          호출 {tool.calls ?? 0}
        </Badge>
      </div>
      <p className="text-muted-foreground line-clamp-1 h-4 text-xs">{tool.description || "(설명 없음)"}</p>
      <div className="mt-auto flex flex-wrap gap-1 pt-1">
        {tool.agents.length === 0 && <span className="text-muted-foreground text-[10px]">(연결된 에이전트 없음)</span>}
        {tool.agents.map((a) => (
          <Badge key={a} variant="outline" className="px-1.5 py-0 text-[10px]">
            {a}
          </Badge>
        ))}
      </div>
    </button>
  );
}

export default function ToolsPage() {
  const [tools, setTools] = React.useState<Tool[]>([]);
  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [captureOn, setCaptureOn] = React.useState(false);
  const [selectedKey, setSelectedKey] = React.useState<string | null>(null);
  const [customEdit, setCustomEdit] = React.useState<Tool | "new" | null>(null);

  const reload = React.useCallback(() => {
    api
      .tools()
      .then(setTools)
      .catch(() => setTools([]));
  }, []);
  React.useEffect(() => {
    reload();
    api
      .agents()
      .then(setAgents)
      .catch(() => {
        // 불러오지 못하면 이전 상태를 그대로 둔다(기존 동작). 오류 알림은 띄우지 않는다.
      });
    api
      .settings()
      .then((s) => setCaptureOn(!!s.traffic_capture))
      .catch(() => {
        // 불러오지 못하면 이전 상태를 그대로 둔다(기존 동작). 오류 알림은 띄우지 않는다.
      });
  }, [reload]);

  const [query, setQuery] = React.useState("");

  const selected = tools.find((t) => t.key === selectedKey) ?? null;
  const matchTool = React.useCallback(
    (t: Tool) => {
      const q = query.trim().toLowerCase();
      if (!q) return true;
      return (
        t.key.toLowerCase().includes(q) ||
        t.description.toLowerCase().includes(q) ||
        t.agents.some((a) => a.toLowerCase().includes(q))
      );
    },
    [query],
  );
  const systemTools = tools.filter((t) => t.system && matchTool(t));
  const customTools = tools.filter((t) => !t.system && matchTool(t));
  const allSystemCount = tools.filter((t) => t.system).length;
  const allCustomCount = tools.filter((t) => !t.system).length;
  // clicking a built-in tool opens the binding drawer; a custom tool opens its full editor.
  const openTool = (t: Tool) => (t.system ? setSelectedKey(t.key) : setCustomEdit(t));

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">도구</h1>
          <p className="text-muted-foreground text-sm">
            시스템 도구의 설명·연결과 사용자 지정 도구(command/script/http)
          </p>
        </div>
        <div className="relative w-64">
          <SearchIcon className="text-muted-foreground absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2" />
          <Input
            className="h-8 pl-8 text-sm"
            placeholder="도구 이름, 설명, 에이전트 검색…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
      </div>

      <Tabs defaultValue="system">
        <TabsList>
          <TabsTrigger value="system">시스템 도구</TabsTrigger>
          <TabsTrigger value="custom">사용자 지정 도구</TabsTrigger>
        </TabsList>

        <TabsContent value="system">
          <Card>
            <CardHeader>
              <CardTitle>시스템 도구</CardTitle>
              <CardDescription>
                {query.trim()
                  ? `${systemTools.length} / ${allSystemCount}개 일치. 카드를 눌러 설명, 파라미터 기본값, 에이전트 연결을 편집합니다`
                  : `총 ${allSystemCount}개. 카드를 눌러 설명, 파라미터 기본값, 에이전트 연결을 편집합니다`}
              </CardDescription>
            </CardHeader>
            <CardContent>
              {systemTools.length === 0 ? (
                <p className="text-muted-foreground py-6 text-center text-sm">
                  {query.trim()
                    ? "일치하는 시스템 도구가 없습니다"
                    : "(시스템 도구가 없습니다. 백엔드 seed를 기다리는 중)"}
                </p>
              ) : (
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                  {systemTools.map((t) => (
                    <ToolGridCard key={t.key} tool={t} onClick={() => openTool(t)} />
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="custom">
          <Card>
            <CardHeader>
              <div className="flex items-start justify-between gap-3">
                <div>
                  <CardTitle>사용자 지정 도구</CardTitle>
                  <CardDescription>
                    {query.trim()
                      ? `shell/command/script/http, ${customTools.length} / ${allCustomCount}개 일치. 카드를 눌러 편집합니다`
                      : `shell(bash 선언) / command(명령) / script(Python) / http(API), 총 ${allCustomCount}개. 카드를 눌러 편집합니다`}
                  </CardDescription>
                </div>
                <Button size="sm" onClick={() => setCustomEdit("new")}>
                  <PlusIcon /> 새 사용자 지정 도구
                </Button>
              </div>
            </CardHeader>
            <CardContent>
              {customTools.length === 0 ? (
                <p className="text-muted-foreground py-6 text-center text-sm">
                  {query.trim()
                    ? "일치하는 사용자 지정 도구가 없습니다"
                    : "(사용자 지정 도구가 없습니다. 오른쪽 위의 「새 사용자 지정 도구」를 누르세요)"}
                </p>
              ) : (
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                  {customTools.map((t) => (
                    <ToolGridCard key={t.key} tool={t} onClick={() => openTool(t)} />
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </TabsContent>
      </Tabs>

      <Sheet open={!!selected} onOpenChange={(o) => !o && setSelectedKey(null)}>
        <SheetContent side="right" className="gap-0 p-0 data-[side=right]:w-[30vw] data-[side=right]:sm:max-w-[30vw]">
          {selected && (
            <>
              <SheetHeader className="px-4">
                <SheetTitle className="font-mono">{selected.key}</SheetTitle>
                <SheetDescription>설명, 파라미터 기본값, 에이전트 연결 편집</SheetDescription>
              </SheetHeader>
              <ToolEditor
                key={selected.key}
                tool={selected}
                agents={agents}
                captureOn={captureOn}
                onSaved={reload}
                onClose={() => setSelectedKey(null)}
              />
            </>
          )}
        </SheetContent>
      </Sheet>

      <CustomToolDialog
        edit={customEdit}
        agents={agents}
        onClose={() => setCustomEdit(null)}
        onSaved={() => {
          setCustomEdit(null);
          reload();
        }}
      />
    </div>
  );
}

// ---- 사용자 지정 도구 편집기 ----

type ExecState = {
  command: string;
  code: string;
  method: string;
  url: string;
  headers: string;
  body: string;
  timeout_ms: string;
  proxy: string;
  use_recording_proxy: boolean;
};

function CustomToolDialog({
  edit,
  agents,
  onClose,
  onSaved,
}: {
  edit: Tool | "new" | null;
  agents: Agent[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const isNew = edit === "new";
  const tool = edit && edit !== "new" ? edit : null;
  const [key, setKey] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [kind, setKind] = React.useState<"shell" | "command" | "script" | "http">("shell");
  const [ex, setEx] = React.useState<ExecState>({
    command: "",
    code: "",
    method: "GET",
    url: "",
    headers: "",
    body: "",
    timeout_ms: "",
    proxy: "",
    use_recording_proxy: false,
  });
  const [schemaText, setSchemaText] = React.useState("");
  const [bound, setBound] = React.useState<string[]>([]);
  const [deferred, setDeferred] = React.useState(false);
  const [enabled, setEnabled] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [paramsText, setParamsText] = React.useState("");
  const [testing, setTesting] = React.useState(false);
  const [testResult, setTestResult] = React.useState<{ output: string; is_error: boolean } | null>(null);

  // (re)load form state when opening.
  React.useEffect(() => {
    if (!edit) return;
    setParamsText("");
    setTestResult(null);
    if (edit === "new") {
      setKey("");
      setDescription("");
      setKind("shell");
      setEx({
        command: "",
        code: "",
        method: "GET",
        url: "",
        headers: "",
        body: "",
        timeout_ms: "",
        proxy: "",
        use_recording_proxy: false,
      });
      setSchemaText("");
      setBound([]);
      setDeferred(false);
      setEnabled(true);
      return;
    }
    const t = edit;
    const e = (t.exec ?? {}) as Record<string, unknown>;
    setKey(t.key);
    setDescription(t.description);
    setKind((t.kind as "shell" | "command" | "script" | "http") ?? "shell");
    setEx({
      command: String(e.command ?? ""),
      code: String(e.code ?? ""),
      method: String(e.method ?? "GET"),
      url: String(e.url ?? ""),
      headers: e.headers ? JSON.stringify(e.headers, null, 2) : "",
      body: String(e.body ?? ""),
      timeout_ms: e.timeout_ms ? String(e.timeout_ms) : "",
      proxy: String(e.proxy ?? ""),
      use_recording_proxy: !!e.use_recording_proxy,
    });
    setSchemaText(t.schema && Object.keys(t.schema).length ? JSON.stringify(t.schema, null, 2) : "");
    setBound(t.agents ?? []);
    setDeferred(!!t.deferred);
    setEnabled(t.enabled);
  }, [edit]);

  const toggleAgent = (k: string) => setBound((b) => (b.includes(k) ? b.filter((x) => x !== k) : [...b, k]));

  function buildExec(): Record<string, unknown> {
    if (kind === "shell") return {};
    const t = ex.timeout_ms ? Number(ex.timeout_ms) : undefined;
    if (kind === "command") return { command: ex.command, timeout_ms: t };
    if (kind === "script") return { code: ex.code, timeout_ms: t };
    let headers: Record<string, string> = {};
    if (ex.headers.trim()) {
      try {
        headers = JSON.parse(ex.headers);
      } catch {
        /* validated on save */
      }
    }
    return {
      method: ex.method,
      url: ex.url,
      headers,
      body: ex.body,
      timeout_ms: t,
      proxy: ex.proxy,
      use_recording_proxy: ex.use_recording_proxy,
    };
  }

  async function save() {
    if (isNew && !/^[a-z][a-z0-9_]*$/.test(key.trim())) {
      toast.error("key는 영문 소문자로 시작하고 영문 소문자·숫자·밑줄만 쓸 수 있습니다");
      return;
    }
    let schema: Record<string, unknown> = {};
    if (schemaText.trim()) {
      try {
        schema = JSON.parse(schemaText);
      } catch {
        toast.error("파라미터 JSON Schema 형식이 잘못됐습니다");
        return;
      }
    }
    if (kind === "http" && ex.headers.trim()) {
      try {
        JSON.parse(ex.headers);
      } catch {
        toast.error("headers JSON 형식이 잘못됐습니다");
        return;
      }
    }
    if (kind === "http") {
      const props = (schema as { properties?: Record<string, unknown> }).properties;
      if (!props || Object.keys(props).length === 0) {
        toast.error("http 도구에는 파라미터 JSON Schema가 있어야 합니다(properties 포함, 비워 둘 수 없음)");
        return;
      }
    }
    setSaving(true);
    const payload = {
      description,
      schema: kind === "shell" ? {} : schema,
      agents: bound,
      enabled,
      kind,
      exec: buildExec(),
      deferred: kind === "shell" ? false : deferred,
    };
    try {
      if (isNew) await api.createCustomTool({ key: key.trim(), ...payload });
      else await api.updateCustomTool(tool!.key, payload);
      toast.success(isNew ? "사용자 지정 도구를 만들었습니다" : "저장했습니다");
      onSaved();
    } catch (e) {
      toast.error("저장하지 못했습니다: " + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }
  async function del() {
    if (!tool) return;
    try {
      await api.deleteCustomTool(tool.key);
      toast.success("삭제했습니다");
      onSaved();
    } catch (e) {
      toast.error("삭제하지 못했습니다: " + (e as Error).message);
    }
  }
  // runTest dry-runs the CURRENT form (unsaved) with the sample params, so a
  // template/script/request can be debugged before saving.
  async function runTest() {
    let params: Record<string, unknown> = {};
    if (paramsText.trim()) {
      try {
        params = JSON.parse(paramsText);
      } catch {
        toast.error("테스트 파라미터 JSON 형식이 잘못됐습니다");
        return;
      }
    }
    if (kind === "http" && ex.headers.trim()) {
      try {
        JSON.parse(ex.headers);
      } catch {
        toast.error("headers JSON 형식이 잘못됐습니다");
        return;
      }
    }
    setTesting(true);
    setTestResult(null);
    try {
      const r = await api.testCustomTool({ kind, exec: buildExec(), params });
      setTestResult(r);
    } catch (e) {
      setTestResult({ output: (e as Error).message, is_error: true });
    } finally {
      setTesting(false);
    }
  }

  return (
    <Sheet open={!!edit} onOpenChange={(o) => !o && onClose()}>
      <SheetContent
        side="right"
        className="flex flex-col gap-0 p-0 data-[side=right]:w-[45vw] data-[side=right]:sm:max-w-[45vw] data-[side=right]:min-w-[480px]"
      >
        <SheetHeader className="px-4">
          <SheetTitle>{isNew ? "새 사용자 지정 도구" : `${tool?.key} 편집`}</SheetTitle>
          <SheetDescription>
            shell = bash 환경 선언(이름과 설명만 쓰면 모델에 bash로 호출할 수 있다고 알림). command/script/http는 실행
            명세를 써야 합니다.
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          <div className="grid gap-1.5">
            <Label className="text-xs">Key</Label>
            <Input
              className="font-mono"
              placeholder="예: nmap_scan"
              value={key}
              disabled={!isNew}
              onChange={(e) => setKey(e.target.value)}
            />
          </div>
          <div className="grid gap-1.5">
            <Label className="text-xs">설명(모델에 보냄)</Label>
            <Textarea rows={2} value={description} onChange={(e) => setDescription(e.target.value)} />
          </div>

          <div className="grid gap-1.5">
            <Label className="text-xs">유형</Label>
            <Select value={kind} onValueChange={(v) => setKind(v as "shell" | "command" | "script" | "http")}>
              <SelectTrigger className="w-56">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="shell">shell(bash 환경 선언)</SelectItem>
                <SelectItem value="command">command(shell 명령 템플릿)</SelectItem>
                <SelectItem value="script">script(Python 스크립트)</SelectItem>
                <SelectItem value="http">http(API 요청)</SelectItem>
              </SelectContent>
            </Select>
            {kind === "shell" && (
              <p className="text-muted-foreground text-xs">
                nmap, sqlmap, ffuf처럼 잘 알려진 도구에 알맞습니다. 모델이 이미 쓰는 법을 알므로 "bash에서 호출할 수
                있다"고만 선언하면 됩니다. 이름과 설명은 Bash 도구 설명 뒤에 붙습니다.
              </p>
            )}
          </div>

          {kind === "command" && (
            <div className="grid gap-1.5">
              <Label className="text-xs">
                명령 템플릿(자리표시자 {"{param}"}. 예: nmap -p {"{ports}"} {"{target}"})
              </Label>
              <Textarea
                className="font-mono text-xs"
                rows={2}
                value={ex.command}
                onChange={(e) => setEx({ ...ex, command: e.target.value })}
              />
            </div>
          )}
          {kind === "script" && (
            <div className="grid gap-1.5">
              <Label className="text-xs">Python 본문(파라미터는 stdin JSON / os.environ["TOOL_X"]로 받음)</Label>
              <Textarea
                className="font-mono text-xs"
                rows={10}
                value={ex.code}
                placeholder={"import json,sys\nargs=json.load(sys.stdin)\nprint(...)"}
                onChange={(e) => setEx({ ...ex, code: e.target.value })}
              />
            </div>
          )}
          {kind === "http" && (
            <div className="grid gap-2">
              <div className="flex gap-2">
                <div className="grid gap-1.5">
                  <Label className="text-xs">Method</Label>
                  <Input
                    className="w-24"
                    value={ex.method}
                    onChange={(e) => setEx({ ...ex, method: e.target.value })}
                  />
                </div>
                <div className="grid flex-1 gap-1.5">
                  <Label className="text-xs">URL({"{param}"} 사용 가능)</Label>
                  <Input
                    className="font-mono text-xs"
                    value={ex.url}
                    onChange={(e) => setEx({ ...ex, url: e.target.value })}
                  />
                </div>
              </div>
              <div className="grid gap-1.5">
                <Label className="text-xs">Headers(JSON, {"{param}"} 사용 가능)</Label>
                <Textarea
                  className="font-mono text-xs"
                  rows={2}
                  value={ex.headers}
                  placeholder={'{"Authorization": "Bearer {token}"}'}
                  onChange={(e) => setEx({ ...ex, headers: e.target.value })}
                />
              </div>
              <div className="grid gap-1.5">
                <Label className="text-xs">Body({"{param}"} 사용 가능)</Label>
                <Textarea
                  className="font-mono text-xs"
                  rows={2}
                  value={ex.body}
                  onChange={(e) => setEx({ ...ex, body: e.target.value })}
                />
              </div>
              <div className="flex items-center gap-4">
                <div className="grid gap-1.5">
                  <Label className="text-xs">프록시 URL(비우면 직접 연결)</Label>
                  <Input
                    className="font-mono text-xs w-56"
                    value={ex.proxy}
                    onChange={(e) => setEx({ ...ex, proxy: e.target.value })}
                  />
                </div>
                <label className="mt-4 flex items-center gap-2 text-sm">
                  <Checkbox
                    checked={ex.use_recording_proxy}
                    onCheckedChange={(v) => setEx({ ...ex, use_recording_proxy: !!v })}
                  />
                  기록 프록시 사용
                </label>
              </div>
            </div>
          )}

          {kind !== "shell" && (
            <div className="flex items-center gap-3">
              <div className="grid gap-1.5">
                <Label className="text-xs">시간 제한(ms, 비우면 기본값)</Label>
                <Input
                  type="number"
                  className="w-32"
                  value={ex.timeout_ms}
                  onChange={(e) => setEx({ ...ex, timeout_ms: e.target.value })}
                />
              </div>
            </div>
          )}

          {kind !== "shell" && (
            <div className="grid gap-1.5">
              <Label className="text-xs">
                파라미터 JSON Schema
                {kind === "http"
                  ? "(http 도구는 필수, properties 포함)"
                  : "(비워 두면 {args} 하나만 받는 기본 스키마를 자동으로 씀)"}
              </Label>
              <Textarea
                className="font-mono text-xs"
                rows={4}
                value={schemaText}
                placeholder={'{"type":"object","properties":{"target":{"type":"string"}},"required":["target"]}'}
                onChange={(e) => setSchemaText(e.target.value)}
              />
            </div>
          )}

          <div className="grid gap-1.5">
            <Label className="text-muted-foreground text-xs">에이전트 연결</Label>
            <div className="flex flex-wrap gap-3">
              {agents.map((a) => (
                <label key={a.key} className="flex items-center gap-2 text-sm">
                  <Checkbox checked={bound.includes(a.key)} onCheckedChange={() => toggleAgent(a.key)} />
                  {a.name}
                  <span className="text-muted-foreground font-mono text-xs">{a.key}</span>
                </label>
              ))}
            </div>
          </div>

          <div className="flex items-center gap-6">
            <label className="flex items-center gap-2 text-sm">
              <Switch checked={enabled} onCheckedChange={setEnabled} /> 사용
            </label>
            {kind !== "shell" && (
              <label className="flex items-center gap-2 text-sm">
                <Switch checked={deferred} onCheckedChange={setDeferred} /> deferred(잘 쓰지 않는 도구가 많을 때만 켜기)
              </label>
            )}
          </div>

          {kind !== "shell" && (
            <div className="grid gap-1.5 rounded-md border p-3">
              <Label className="text-xs font-medium">테스트 실행(현재 폼으로 실행하며 저장하지 않음)</Label>
              <Textarea
                className="font-mono text-xs"
                rows={2}
                value={paramsText}
                placeholder={'예시 파라미터 JSON. 예: {"target":"example.com"}'}
                onChange={(e) => setParamsText(e.target.value)}
              />
              <div>
                <Button size="sm" variant="outline" onClick={runTest} disabled={testing}>
                  <PlayIcon /> {testing ? "실행 중…" : "테스트 실행"}
                </Button>
              </div>
              {testResult && (
                <pre
                  className={
                    "max-h-64 overflow-auto whitespace-pre-wrap break-words rounded bg-muted p-2 font-mono text-xs " +
                    (testResult.is_error ? "text-destructive" : "")
                  }
                >
                  {testResult.output || "(출력 없음)"}
                </pre>
              )}
            </div>
          )}
        </div>

        <Separator />
        <div className="flex items-center gap-2 p-4">
          <Button size="sm" onClick={save} disabled={saving}>
            <SaveIcon /> {isNew ? "만들기" : "저장"}
          </Button>
          {!isNew && (
            <Button size="sm" variant="outline" className="text-destructive" onClick={del}>
              <Trash2Icon /> 삭제
            </Button>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}
