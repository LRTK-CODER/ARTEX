"use client";

import * as React from "react";

import Link from "next/link";

import {
  DndContext,
  type DragEndEvent,
  DragOverlay,
  type DragStartEvent,
  KeyboardSensor,
  PointerSensor,
  useDraggable,
  useDroppable,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import {
  ArchiveIcon,
  ArrowDownIcon,
  ArrowUpDownIcon,
  ArrowUpIcon,
  ChevronRightIcon,
  EyeIcon,
  FolderInputIcon,
  GripVerticalIcon,
  Loader2Icon,
  PaperclipIcon,
  PauseIcon,
  PencilIcon,
  PinIcon,
  PinOffIcon,
  PlayIcon,
  PlusIcon,
  SaveIcon,
  SearchIcon,
  SlidersHorizontalIcon,
  StarIcon,
  TagsIcon,
  Trash2Icon,
  Undo2Icon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";

import { AssetInterceptRulesEditor } from "@/components/asset-intercept-rules-editor";
import { StatusBadge } from "@/components/status-badge";
import { TablePagination } from "@/components/table-pagination";
import { TaskLLMProfileChain } from "@/components/task-llm-profile-chain";
import { TaskTemplateControls } from "@/components/task-template-controls";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Combobox,
  ComboboxChip,
  ComboboxChips,
  ComboboxChipsInput,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxItem,
  ComboboxList,
  ComboboxValue,
} from "@/components/ui/combobox";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import {
  Field,
  FieldContent,
  FieldDescription,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Item, ItemActions, ItemContent, ItemDescription, ItemGroup, ItemMedia, ItemTitle } from "@/components/ui/item";
import { Label } from "@/components/ui/label";
import { Progress } from "@/components/ui/progress";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import {
  Sheet,
  SheetClose,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { api } from "@/lib/api";
import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";
import { type SortDirection, useStoredSortPreference } from "@/lib/sort-preference";
import type {
  AssetInterceptRuleInput,
  ChatAttachment,
  Company,
  DeleteTaskOptions,
  DeleteTaskResult,
  LLMProfile,
  Task,
  TaskArchive,
  TaskArchiveState,
  TaskCategory,
  TaskStatus,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// fmtBytes renders a human file size for the upload manifest.
function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

// UPLOAD_MARKER labels the auto-appended block of uploaded-file paths inside the task
// description, so re-uploads append under the same block instead of adding a new header.
const UPLOAD_MARKER = "【업로드한 파일(절대 경로)】";

// appendUploads folds newly-uploaded files' ABSOLUTE paths into the description as a
// Read/Bash-friendly manifest — the worker opens them by path. Keeps one marked block:
// first upload adds the header, later uploads append bullets under it.
function appendUploads(desc: string, atts: ChatAttachment[]): string {
  const bullets = atts.map((a) => `- ${a.abs ?? a.path}（${fmtBytes(a.size)}）`).join("\n");
  if (desc.includes(UPLOAD_MARKER)) {
    return `${desc.replace(/\s*$/, "")}\n${bullets}\n`;
  }
  const head = desc.trim() ? `${desc.replace(/\s*$/, "")}\n\n` : "";
  return `${head}${UPLOAD_MARKER} 워커가 Read/Bash로 경로를 열 수 있습니다:\n${bullets}\n`;
}

// POLL_MS is the task-list refresh interval. Task state moves on the server (planner /
// worker), so the list has to be pulled; 10s is plenty for status / progress / token changes.
const POLL_MS = 10_000;
const MAX_SOURCE_TASKS = 8;

// fmtTokens renders a compact token count (1234 → 1.2k, 2_000_000 → 2M).
function fmtTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1) + "M";
  if (n >= 1000) return (n / 1000).toFixed(n >= 10000 ? 0 : 1) + "k";
  return String(n);
}

// fmtDuration renders a run duration in seconds as a compact human string.
function fmtDuration(sec: number): string {
  if (sec <= 0) return "—";
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

// taskDuration is the task's run duration in seconds: created → now while running,
// created → completion for a finished task, else created → last activity. 0 when it
// never ran (no activity yet).
function taskDuration(task: Task, nowSec: number): number {
  const start = task.created_unix ?? 0;
  if (!start) return 0;
  const end =
    task.status === "running"
      ? nowSec
      : task.completed_unix && task.completed_unix > 0
        ? task.completed_unix
        : (task.last_activity_unix ?? 0);
  return end > start ? end - start : 0;
}

// deleteDetails takes only the countable part of the result so callers can pass an
// aggregate accumulated over a bulk delete.
type DeleteCounts = Omit<DeleteTaskResult, "deleted" | "cleanup_warning">;

function deleteDetails(result: DeleteCounts): string[] {
  const details: string[] = [];
  if (result.assets_deleted > 0) details.push(`자산 ${result.assets_deleted}건 삭제`);
  if (result.assets_detached > 0) details.push(`공유 자산 연결 ${result.assets_detached}건 해제`);
  if (result.traffic_deleted > 0) details.push(`트래픽 ${result.traffic_deleted}건 삭제`);
  if (result.files_deleted) details.push("작업 파일 삭제");
  if (result.findings_deleted > 0) details.push(`취약점 ${result.findings_deleted}건 삭제`);
  if (result.llm_records_deleted > 0) details.push(`LLM 요청/응답 기록 ${result.llm_records_deleted}건 삭제`);
  return details;
}

function deleteSummary(result: DeleteTaskResult): string {
  const details = deleteDetails(result);
  return details.length > 0 ? `작업을 삭제했습니다(${details.join(", ")}).` : "작업을 삭제했습니다.";
}

// fmtDateTime renders a unix-seconds timestamp as a compact local date-time
// (MM-DD HH:mm), or "—" when unset.
function fmtDateTime(unix?: number): string {
  if (!unix || unix <= 0) return "—";
  const d = new Date(unix * 1000);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

const STATUS_OPTIONS: { value: TaskStatus; label: string }[] = [
  { value: "created", label: "생성됨" },
  { value: "queued", label: "대기 중" },
  { value: "running", label: "실행 중" },
  { value: "paused", label: "일시 중지됨" },
  { value: "done", label: "완료" },
  { value: "failed", label: "실패" },
  { value: "timeout", label: "시간 초과" },
];

// Select는 빈 문자열 value를 받지 않으므로 필터, 새 작업 양식, 일괄 이동에서 "미분류"는
// 모두 이 센티널 값을 쓰고, 제출할 때 백엔드의 null로 바꾼다.
const UNCATEGORIZED_VALUE = "uncategorized";

// 일시 중지 가능 = 종료 상태가 아니고 일시 중지되지 않음. 백엔드 applyTaskControlWithCause의 조건과 같다
// (done/failed/timeout이 종료 상태). paused여야 재개할 수 있다. 행 버튼과 일괄 제어가 이 판단을 함께 써서
// 한쪽은 눌리고 다른 쪽은 안 눌리는 어긋남이 생기지 않는다.
const PAUSABLE_STATUSES = new Set<TaskStatus>(["created", "queued", "running"]);
const ARCHIVABLE_STATUSES = new Set<TaskStatus>(["paused", "done", "failed", "timeout"]);

function taskControlAction(status: TaskStatus): "pause" | "resume" | null {
  if (status === "paused") return "resume";
  if (PAUSABLE_STATUSES.has(status)) return "pause";
  return null;
}

type TaskSortField = "id" | "created" | "duration" | "status";

const TASK_SORT_FIELDS: readonly TaskSortField[] = ["id", "created", "duration", "status"];
const TASK_SORT_PREFERENCE_KEY = "artex_task_list_sort";
const TASK_FILTER_PREFERENCE_KEY = "artex_task_list_filters";

const TASK_STATUS_RANK = new Map(STATUS_OPTIONS.map((option, index) => [option.value, index]));

function taskIsPinned(task: Task): boolean {
  return task.pinned ?? Boolean(task.pinned_at);
}

function compareTaskIDs(left: string, right: string): number {
  return left.localeCompare(right, undefined, { numeric: true, sensitivity: "base" });
}

function taskCreatedUnix(task: Task): number {
  if (task.created_unix) return task.created_unix;
  const parsed = Date.parse(task.created_at);
  return Number.isNaN(parsed) ? 0 : Math.floor(parsed / 1000);
}

function compareTasks(left: Task, right: Task, field: TaskSortField, direction: SortDirection, nowSec: number): number {
  const leftPinned = taskIsPinned(left);
  const rightPinned = taskIsPinned(right);
  if (leftPinned !== rightPinned) return leftPinned ? -1 : 1;
  if (leftPinned && left.pinned_at !== right.pinned_at) {
    return (right.pinned_at ?? "").localeCompare(left.pinned_at ?? "");
  }

  let compared = 0;
  switch (field) {
    case "created":
      compared = taskCreatedUnix(left) - taskCreatedUnix(right);
      break;
    case "duration":
      compared = taskDuration(left, nowSec) - taskDuration(right, nowSec);
      break;
    case "status":
      compared = (TASK_STATUS_RANK.get(left.status) ?? 0) - (TASK_STATUS_RANK.get(right.status) ?? 0);
      break;
    default:
      compared = compareTaskIDs(left.id, right.id);
  }
  if (compared !== 0) return direction === "asc" ? compared : -compared;
  return -compareTaskIDs(left.id, right.id);
}

export default function TasksPage() {
  const [activeTab, setActiveTab] = React.useState("current");
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [categories, setCategories] = React.useState<TaskCategory[]>([]);
  const [categoriesLoaded, setCategoriesLoaded] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [statusFilter, setStatusFilter] = React.useState<TaskStatus | "all">("all");
  const [categoryFilter, setCategoryFilter] = React.useState("all");
  const [filtersHydrated, setFiltersHydrated] = React.useState(false);
  const [sortPreference, setSortPreference] = useStoredSortPreference(
    TASK_SORT_PREFERENCE_KEY,
    TASK_SORT_FIELDS,
    "id",
    "desc",
  );
  const { field: sortField, direction: sortDirection } = sortPreference;
  const [page, setPage] = React.useState(1);
  const [pageSize, setPageSize] = React.useState(20);
  const [nowSec, setNowSec] = React.useState(() => Math.floor(Date.now() / 1000));
  const [batchControlling, setBatchControlling] = React.useState<"pause" | "resume" | null>(null);
  const [movingCategory, setMovingCategory] = React.useState(false);

  React.useEffect(() => {
    const raw = getLocalStorageValue(TASK_FILTER_PREFERENCE_KEY);
    if (raw) {
      try {
        const parsed = JSON.parse(raw) as { status?: unknown; category?: unknown };
        if (parsed.status === "all" || STATUS_OPTIONS.some((option) => option.value === parsed.status)) {
          setStatusFilter(parsed.status as TaskStatus | "all");
        }
        if (
          typeof parsed.category === "string" &&
          (parsed.category === "all" || parsed.category === UNCATEGORIZED_VALUE || /^\d+$/.test(parsed.category))
        ) {
          setCategoryFilter(parsed.category);
        }
      } catch {
        // Ignore malformed or legacy preferences and retain the defaults.
      }
    }
    setFiltersHydrated(true);
  }, []);

  React.useEffect(() => {
    if (!filtersHydrated) return;
    setLocalStorageValue(
      TASK_FILTER_PREFERENCE_KEY,
      JSON.stringify({ status: statusFilter, category: categoryFilter }),
    );
  }, [categoryFilter, filtersHydrated, statusFilter]);

  const filtered = React.useMemo(() => {
    const q = query.trim().toLowerCase();
    return tasks.filter((t) => {
      if (statusFilter !== "all" && t.status !== statusFilter) return false;
      if (categoryFilter === UNCATEGORIZED_VALUE && t.category_id != null) return false;
      if (
        categoryFilter !== "all" &&
        categoryFilter !== UNCATEGORIZED_VALUE &&
        String(t.category_id) !== categoryFilter
      )
        return false;
      if (!q) return true;
      return (
        (t.name ?? "").toLowerCase().includes(q) ||
        (t.category_name ?? "").toLowerCase().includes(q) ||
        t.description.toLowerCase().includes(q) ||
        t.goal.toLowerCase().includes(q) ||
        t.id.toLowerCase().includes(q)
      );
    });
  }, [tasks, query, statusFilter, categoryFilter]);

  const sortNowSec = sortField === "duration" ? nowSec : 0;
  const ordered = React.useMemo(() => {
    return [...filtered].sort((left, right) => compareTasks(left, right, sortField, sortDirection, sortNowSec));
  }, [filtered, sortDirection, sortField, sortNowSec]);

  const sortTasksBy = React.useCallback(
    (field: TaskSortField) => {
      setSortPreference((current) => {
        if (current.field !== field) return { field, direction: "desc" };
        return { field, direction: current.direction === "asc" ? "desc" : "asc" };
      });
    },
    [setSortPreference],
  );

  // reset to page 1 whenever filters or ordering change
  // biome-ignore lint/correctness/useExhaustiveDependencies: both filters intentionally reset pagination.
  React.useEffect(() => {
    setPage(1);
  }, [query, statusFilter, categoryFilter, sortField, sortDirection]);

  const paginated = React.useMemo(
    () => ordered.slice((page - 1) * pageSize, page * pageSize),
    [ordered, page, pageSize],
  );

  // 다중 선택 삭제: 선택은 페이지 이동·필터를 넘어 유지되고, 작업이 실제로 사라졌을 때(삭제됐거나 백엔드가 더 돌려주지 않을 때)만 줄어든다.
  const [selectedIds, setSelectedIds] = React.useState<Set<string>>(() => new Set());

  React.useEffect(() => {
    setSelectedIds((prev) => {
      if (prev.size === 0) return prev;
      const live = new Set(tasks.map((t) => t.id));
      const next = new Set([...prev].filter((id) => live.has(id)));
      return next.size === prev.size ? prev : next;
    });
  }, [tasks]);

  const toggleSelected = React.useCallback((id: string, checked: boolean) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);

  const pageIds = React.useMemo(() => paginated.map((t) => t.id), [paginated]);
  const pageSelectedCount = React.useMemo(
    () => pageIds.filter((id) => selectedIds.has(id)).length,
    [pageIds, selectedIds],
  );
  let headerChecked: boolean | "indeterminate" = false;
  if (pageIds.length > 0 && pageSelectedCount === pageIds.length) {
    headerChecked = true;
  } else if (pageSelectedCount > 0) {
    headerChecked = "indeterminate";
  }

  const toggleSelectedPage = React.useCallback(
    (checked: boolean) => {
      setSelectedIds((prev) => {
        const next = new Set(prev);
        for (const id of pageIds) {
          if (checked) next.add(id);
          else next.delete(id);
        }
        return next;
      });
    },
    [pageIds],
  );

  // lastRef holds the previous poll's serialized payload: the list is re-fetched every
  // POLL_MS but usually comes back unchanged, and setTasks on an identical payload would
  // re-render the whole page for nothing. Bail out when it matches.
  const lastRef = React.useRef<string>("");

  const load = React.useCallback(() => {
    api
      .tasks()
      .then((r) => {
        const next = r.tasks.map((t) => (t.id === r.active ? { ...t, active: true } : t));
        const sig = JSON.stringify(next);
        if (sig === lastRef.current) return;
        lastRef.current = sig;
        setTasks(next);
      })
      .catch(() => {
        // Polling is best-effort; the next interval retries automatically.
      });
  }, []);

  const loadCategories = React.useCallback(() => {
    api
      .taskCategories()
      .then((next) => {
        setCategories(next);
        setCategoriesLoaded(true);
      })
      .catch(() => {
        // Category management remains retryable without blocking the task list.
      });
  }, []);

  React.useEffect(() => {
    load();
    loadCategories();
    const i = setInterval(load, POLL_MS);
    return () => clearInterval(i);
  }, [load, loadCategories]);

  const refreshCategoriesAndTasks = React.useCallback(() => {
    lastRef.current = "";
    loadCategories();
    load();
  }, [load, loadCategories]);

  const applyTaskCategoryMove = React.useCallback(
    (taskID: string, category: TaskCategory | null) => {
      setTasks((current) => {
        const next = current.map((task) =>
          task.id === taskID ? { ...task, category_id: category?.id, category_name: category?.name } : task,
        );
        lastRef.current = JSON.stringify(next);
        return next;
      });
      loadCategories();
    },
    [loadCategories],
  );

  React.useEffect(() => {
    if (!categoriesLoaded) return;
    if (categoryFilter === "all" || categoryFilter === UNCATEGORIZED_VALUE) return;
    if (!categories.some((category) => String(category.id) === categoryFilter)) setCategoryFilter("all");
  }, [categories, categoriesLoaded, categoryFilter]);

  // running 작업이 있을 때만 매초 tick한다. 그 밖에는 "실행 시간"이 고정값이라 헛도는 tick이
  // 표 전체를 괜히 다시 렌더링한다.
  const hasRunning = React.useMemo(() => tasks.some((t) => t.status === "running"), [tasks]);

  // tick every second so running tasks' elapsed time counts up live.
  React.useEffect(() => {
    if (!hasRunning) return;
    setNowSec(Math.floor(Date.now() / 1000));
    const i = setInterval(() => setNowSec(Math.floor(Date.now() / 1000)), 1000);
    return () => clearInterval(i);
  }, [hasRunning]);

  const deleteTask = React.useCallback(
    async (id: string, options: DeleteTaskOptions) => {
      try {
        const result = await api.deleteTask(id, options);
        if (result.cleanup_warning) {
          toast.warning(`${deleteSummary(result)} 일부 외부 데이터를 정리하지 못했습니다: ${result.cleanup_warning}`);
        } else {
          toast.success(deleteSummary(result));
        }
        load();
      } catch (e) {
        toast.error("작업을 삭제하지 못했습니다: " + (e as Error).message);
        throw e;
      }
    },
    [load],
  );

  // controlTask는 행 단위 일시 중지/재개다. 일괄은 controlTasksBatch를, 한 행은 단건 작업 API를 써서
  // "체크한 뒤 일괄 버튼 누르기"를 생략한다. lastRef를 비워 다음 폴링이 같은 응답도 그대로 받게 한다.
  // 그러지 않으면 상태 반영이 중복 제거에 막혀 버튼이 반응하지 않는 것처럼 보인다.
  const controlTask = React.useCallback(
    async (id: string, action: "pause" | "resume") => {
      try {
        const result = await api.controlTask(id, action);
        toast.success(
          action === "pause"
            ? `작업 #${id}을 일시 중지했습니다.`
            : `작업 #${id}을 재개했습니다.${result.queued ? " 대기열에 들어갔습니다." : ""}`,
        );
      } catch (e) {
        toast.error(`작업을 ${action === "pause" ? "일시 중지" : "재개"}하지 못했습니다: ${(e as Error).message}`);
      } finally {
        // 성공 여부와 관계없이 새로 고친다. 실패는 대개 상태가 이미 바뀐 경우라 다시 가져와야 버튼이 맞는 모양으로 돌아온다.
        lastRef.current = "";
        load();
      }
    },
    [load],
  );

  const renameTask = React.useCallback(
    async (task: Task, name: string) => {
      try {
        await api.renameTask(task.id, name);
        toast.success(`작업 #${task.id}의 이름을 바꿨습니다`);
        lastRef.current = "";
        load();
      } catch (error) {
        toast.error(`이름을 바꾸지 못했습니다: ${(error as Error).message}`);
        throw error;
      }
    },
    [load],
  );

  const toggleTaskPinned = React.useCallback(
    async (task: Task) => {
      const pinned = taskIsPinned(task);
      try {
        await api.pinTask(task.id, !pinned);
        toast.success(
          pinned ? `작업 #${task.id}의 상단 고정을 해제했습니다.` : `작업 #${task.id}을 상단에 고정했습니다.`,
        );
        lastRef.current = "";
        load();
      } catch (error) {
        toast.error(
          `${pinned ? "작업 상단 고정을 해제" : "작업을 상단에 고정"}하지 못했습니다: ${(error as Error).message}`,
        );
        throw error;
      }
    },
    [load],
  );

  const queueTaskArchive = React.useCallback(
    async (task: Task) => {
      try {
        await api.archiveTask(task.id);
        toast.success(`작업 #${task.id}을 보관 대기열에 넣었습니다`);
        setActiveTab("archived");
        lastRef.current = "";
        load();
      } catch (error) {
        toast.error(`작업을 보관하지 못했습니다: ${(error as Error).message}`);
        throw error;
      }
    },
    [load],
  );

  // deleteTasks는 선택한 작업을 하나씩 삭제한다. 백엔드에 일괄 API가 없고 한 번의 삭제가 자산·트래픽·파일까지 정리하므로
  // 백엔드에 한꺼번에 부하가 몰리지 않게 순서대로 실행한다. 성공한 것은 선택에서 빼고, 실패한 것은 재시도할 수 있게 남긴다.
  const deleteTasks = React.useCallback(
    async (ids: string[], options: DeleteTaskOptions, onProgress: (done: number) => void) => {
      const total: DeleteCounts = {
        assets_deleted: 0,
        assets_detached: 0,
        traffic_deleted: 0,
        files_deleted: false,
        findings_deleted: 0,
        llm_records_deleted: 0,
      };
      const deleted: string[] = [];
      const failed: { id: string; message: string }[] = [];
      const warnings: string[] = [];

      for (const id of ids) {
        try {
          const r = await api.deleteTask(id, options);
          total.assets_deleted += r.assets_deleted;
          total.assets_detached += r.assets_detached;
          total.traffic_deleted += r.traffic_deleted;
          total.files_deleted = total.files_deleted || r.files_deleted;
          total.findings_deleted += r.findings_deleted;
          total.llm_records_deleted += r.llm_records_deleted;
          if (r.cleanup_warning) warnings.push(`#${id}：${r.cleanup_warning}`);
          deleted.push(id);
        } catch (e) {
          failed.push({ id, message: (e as Error).message });
        }
        onProgress(deleted.length + failed.length);
      }

      if (deleted.length > 0) {
        setSelectedIds((prev) => {
          const next = new Set(prev);
          for (const id of deleted) next.delete(id);
          return next;
        });
        const details = deleteDetails(total);
        const summary =
          `작업 ${deleted.length}개를 삭제했습니다` + (details.length > 0 ? `(${details.join(", ")})` : "") + ".";
        if (warnings.length > 0) {
          toast.warning(`${summary} 일부 외부 데이터를 정리하지 못했습니다: ${warnings.join("; ")}`);
        } else {
          toast.success(summary);
        }
      }
      if (failed.length > 0) {
        const head = failed
          .slice(0, 3)
          .map((f) => `#${f.id}（${f.message}）`)
          .join("；");
        toast.error(`작업 ${failed.length}개를 삭제하지 못했습니다: ${head}${failed.length > 3 ? " 등" : ""}`);
      }
      load();
    },
    [load],
  );

  const selectedTasks = React.useMemo(() => tasks.filter((task) => selectedIds.has(task.id)), [tasks, selectedIds]);
  const pausableTaskIDs = React.useMemo(
    () => selectedTasks.filter((task) => taskControlAction(task.status) === "pause").map((task) => task.id),
    [selectedTasks],
  );
  const resumableTaskIDs = React.useMemo(
    () => selectedTasks.filter((task) => taskControlAction(task.status) === "resume").map((task) => task.id),
    [selectedTasks],
  );
  const archivableTaskIDs = React.useMemo(
    () => selectedTasks.filter((task) => ARCHIVABLE_STATUSES.has(task.status) && !task.queued).map((task) => task.id),
    [selectedTasks],
  );

  const archiveSelectedTasks = React.useCallback(async () => {
    if (archivableTaskIDs.length === 0) return;
    const result = await api.archiveTasks(archivableTaskIDs);
    const succeeded = result.items.filter((item) => item.ok);
    const failed = result.items.filter((item) => !item.ok);
    if (succeeded.length > 0) toast.success(`작업 ${succeeded.length}개를 보관 대기열에 넣었습니다`);
    if (failed.length > 0) {
      toast.error(
        `작업 ${failed.length}개를 보관하지 못했습니다: ${failed
          .slice(0, 3)
          .map((item) => `#${item.id}(${item.error || "상태가 바뀌었습니다"})`)
          .join("；")}`,
      );
    }
    setSelectedIds(new Set());
    setActiveTab("archived");
    lastRef.current = "";
    load();
  }, [archivableTaskIDs, load]);

  const controlSelectedTasks = React.useCallback(
    async (action: "pause" | "resume", ids: string[]) => {
      if (ids.length === 0 || batchControlling) return;
      if (ids.length > 100) {
        toast.error("한 번에 작업을 최대 100개까지 제어할 수 있습니다");
        return;
      }
      setBatchControlling(action);
      try {
        const result = await api.controlTasksBatch(ids, action);
        const succeeded = result.items.filter((item) => item.ok);
        const failed = result.items.filter((item) => !item.ok);
        if (succeeded.length > 0) {
          toast.success(
            action === "pause"
              ? `작업 ${succeeded.length}개를 일시 중지했습니다.`
              : `작업 ${succeeded.length}개를 재개했습니다.${succeeded.some((item) => item.queued) ? " 일부 작업은 대기열에 들어갔습니다." : ""}`,
          );
        }
        if (failed.length > 0) {
          const details = failed
            .slice(0, 3)
            .map((item) => `#${item.id}(${item.error || "상태가 바뀌었습니다"})`)
            .join("；");
          toast.error(`작업 ${failed.length}개를 제어하지 못했습니다: ${details}${failed.length > 3 ? " 등" : ""}`);
        }
        lastRef.current = "";
        load();
      } catch (error) {
        toast.error(
          `작업을 일괄 ${action === "pause" ? "일시 중지" : "재개"}하지 못했습니다: ${(error as Error).message}`,
        );
      } finally {
        setBatchControlling(null);
      }
    },
    [batchControlling, load],
  );

  // 백엔드가 분류 일괄 변경을 한 트랜잭션으로 처리하므로, 실패 항목은 선택한 뒤 삭제된 작업뿐이다.
  const moveSelectedTasksCategory = React.useCallback(
    async (categoryID?: number) => {
      const ids = [...selectedIds];
      if (ids.length === 0 || movingCategory) return;
      if (ids.length > 100) {
        toast.error("한 번에 작업을 최대 100개까지 분류를 바꿀 수 있습니다");
        return;
      }
      setMovingCategory(true);
      try {
        const result = await api.updateTasksCategory(ids, categoryID);
        const succeeded = result.items.filter((item) => item.ok);
        const failed = result.items.filter((item) => !item.ok);
        const target = result.category?.name ?? "미분류";
        if (succeeded.length > 0) {
          toast.success(`작업 ${succeeded.length}개를 "${target}"(으)로 옮겼습니다`);
          setSelectedIds(new Set());
        }
        if (failed.length > 0) {
          const details = failed
            .slice(0, 3)
            .map((item) => `#${item.id}(${item.error || "작업이 없습니다"})`)
            .join("；");
          toast.error(`작업 ${failed.length}개를 옮기지 못했습니다: ${details}${failed.length > 3 ? " 등" : ""}`);
        }
        refreshCategoriesAndTasks();
      } catch (error) {
        toast.error(`분류를 바꾸지 못했습니다: ${(error as Error).message}`);
      } finally {
        setMovingCategory(false);
      }
    },
    [movingCategory, refreshCategoriesAndTasks, selectedIds],
  );

  return (
    <Tabs value={activeTab} onValueChange={setActiveTab} className="gap-4">
      <TabsList className="mx-4 lg:mx-6">
        <TabsTrigger value="current">현재 작업</TabsTrigger>
        <TabsTrigger value="archived">보관됨</TabsTrigger>
      </TabsList>
      <TabsContent value="current">
        <Card>
          <CardContent className="flex flex-col gap-4 px-0 pt-6">
            <div className="flex flex-wrap items-center gap-2 px-4 lg:px-6">
              <div className="relative w-full sm:max-w-xs">
                <SearchIcon className="text-muted-foreground pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2" />
                <Input
                  placeholder="설명 / 목표 / ID 검색"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  className="pl-8"
                />
                {query && (
                  <button
                    type="button"
                    onClick={() => setQuery("")}
                    aria-label="검색어 지우기"
                    className="text-muted-foreground hover:text-foreground absolute top-1/2 right-2 -translate-y-1/2"
                  >
                    <XIcon className="size-4" />
                  </button>
                )}
              </div>
              <Select value={statusFilter} onValueChange={(v) => setStatusFilter(v as TaskStatus | "all")}>
                <SelectTrigger className="w-36">
                  <SelectValue placeholder="상태" />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    <SelectItem value="all">전체 상태</SelectItem>
                    {STATUS_OPTIONS.map((s) => (
                      <SelectItem key={s.value} value={s.value}>
                        {s.label}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
              <Select value={categoryFilter} onValueChange={setCategoryFilter}>
                <SelectTrigger className="w-40">
                  <SelectValue placeholder="작업 분류" />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    <SelectItem value="all">전체 분류</SelectItem>
                    <SelectItem value={UNCATEGORIZED_VALUE}>미분류</SelectItem>
                    {categories.map((category) => (
                      <SelectItem key={category.id} value={String(category.id)}>
                        {category.name}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
              <span className="text-muted-foreground text-xs tabular-nums">
                {filtered.length}/{tasks.length}건
              </span>
              {selectedIds.size > 0 && (
                <>
                  <span className="text-xs tabular-nums">{selectedIds.size}개 선택됨</span>
                  <Button size="sm" variant="ghost" onClick={() => setSelectedIds(new Set())}>
                    선택 해제
                  </Button>
                  {pausableTaskIDs.length > 0 && (
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={batchControlling !== null}
                      onClick={() => void controlSelectedTasks("pause", pausableTaskIDs)}
                    >
                      {batchControlling === "pause" ? (
                        <Spinner data-icon="inline-start" />
                      ) : (
                        <PauseIcon data-icon="inline-start" />
                      )}
                      일시 중지 {pausableTaskIDs.length}
                    </Button>
                  )}
                  {resumableTaskIDs.length > 0 && (
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={batchControlling !== null}
                      onClick={() => void controlSelectedTasks("resume", resumableTaskIDs)}
                    >
                      {batchControlling === "resume" ? (
                        <Spinner data-icon="inline-start" />
                      ) : (
                        <PlayIcon data-icon="inline-start" />
                      )}
                      재개 {resumableTaskIDs.length}
                    </Button>
                  )}
                  {archivableTaskIDs.length > 0 && (
                    <ArchiveConfirmDialog
                      count={archivableTaskIDs.length}
                      onConfirm={archiveSelectedTasks}
                      trigger={
                        <Button size="sm" variant="outline">
                          <ArchiveIcon data-icon="inline-start" />
                          보관 {archivableTaskIDs.length}
                        </Button>
                      }
                    />
                  )}
                  <MoveTasksCategoryDialog
                    categories={categories}
                    count={selectedIds.size}
                    moving={movingCategory}
                    onMove={moveSelectedTasksCategory}
                  />
                  <BulkDeleteTasksDialog ids={[...selectedIds]} onDelete={deleteTasks} />
                </>
              )}
              <ConcurrencySettingsDialog />
              <CategoryManagementSheet
                categories={categories}
                tasks={tasks}
                onChanged={refreshCategoriesAndTasks}
                onTaskMoved={applyTaskCategoryMove}
              />
              <CreateTaskSheet
                tasks={tasks}
                categories={categories}
                onCreated={refreshCategoriesAndTasks}
                onCategoriesChanged={refreshCategoriesAndTasks}
              />
            </div>

            {tasks.length === 0 ? (
              <div className="text-muted-foreground mx-4 flex items-center justify-center rounded-lg border border-dashed py-20 text-sm lg:mx-6">
                작업이 없습니다. 오른쪽 위의 "새 작업"을 눌러 시작하세요.
              </div>
            ) : filtered.length === 0 ? (
              <div className="text-muted-foreground mx-4 flex items-center justify-center rounded-lg border border-dashed py-20 text-sm lg:mx-6">
                일치하는 작업이 없습니다.
              </div>
            ) : (
              <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
                <TableHeader className="[&_tr]:border-t">
                  <TableRow>
                    <TableHead className="w-10">
                      <Checkbox
                        checked={headerChecked}
                        onCheckedChange={(checked) => toggleSelectedPage(checked === true)}
                        aria-label="이 페이지의 작업 모두 선택"
                      />
                    </TableHead>
                    <SortableTaskHead
                      field="id"
                      label="ID"
                      activeField={sortField}
                      direction={sortDirection}
                      className="font-mono"
                      onSort={sortTasksBy}
                    />
                    <TableHead>이름</TableHead>
                    <TableHead>설명</TableHead>
                    <TableHead>목표</TableHead>
                    <SortableTaskHead
                      field="status"
                      label="상태"
                      activeField={sortField}
                      direction={sortDirection}
                      onSort={sortTasksBy}
                    />
                    <TableHead className="text-center">목표 진행률</TableHead>
                    <TableHead className="text-center" title="치명 / 높음 / 중간 / 낮음">
                      취약점 <span className="text-muted-foreground font-normal">치명/높음/중간/낮음</span>
                    </TableHead>
                    <TableHead className="text-center">실행 중 워커</TableHead>
                    <SortableTaskHead
                      field="created"
                      label="생성 시간"
                      activeField={sortField}
                      direction={sortDirection}
                      align="right"
                      onSort={sortTasksBy}
                    />
                    <SortableTaskHead
                      field="duration"
                      label="실행 시간"
                      activeField={sortField}
                      direction={sortDirection}
                      align="right"
                      onSort={sortTasksBy}
                    />
                    <TableHead className="text-right">Token</TableHead>
                    <TableHead className="sticky right-0 z-10 bg-card text-right shadow-[-1px_0_0_0_hsl(var(--border))]">
                      관리
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {paginated.map((task) => (
                    <TaskRow
                      key={task.id}
                      task={task}
                      // running 작업만 nowSec를 받고 나머지 행은 0을 넘긴다. props가 바뀌지 않으므로 memo가 매초 tick으로
                      // 생기는 표 전체 재렌더링을 막고, 실행 중인 행만 시간이 흐른다.
                      nowSec={task.status === "running" ? nowSec : 0}
                      onDelete={deleteTask}
                      onControl={controlTask}
                      onRename={renameTask}
                      onTogglePinned={toggleTaskPinned}
                      onArchive={queueTaskArchive}
                      selected={selectedIds.has(task.id)}
                      onSelectedChange={toggleSelected}
                    />
                  ))}
                </TableBody>
              </Table>
            )}
            <TablePagination
              page={page}
              pageSize={pageSize}
              total={filtered.length}
              onPageChange={setPage}
              onPageSizeChange={(nextPageSize) => {
                setPageSize(nextPageSize);
                setPage(1);
              }}
            />
          </CardContent>
        </Card>
      </TabsContent>
      <TabsContent value="archived">
        <TaskArchivesPanel onChanged={load} />
      </TabsContent>
    </Tabs>
  );
}

function taskSortIcon(active: boolean, direction: SortDirection) {
  if (!active) {
    return <ArrowUpDownIcon className="size-3.5 opacity-40 transition-opacity group-hover/sort:opacity-100" />;
  }
  if (direction === "asc") return <ArrowUpIcon className="size-3.5" />;
  return <ArrowDownIcon className="size-3.5" />;
}

function SortableTaskHead({
  field,
  label,
  activeField,
  direction,
  align = "left",
  className,
  onSort,
}: {
  field: TaskSortField;
  label: string;
  activeField: TaskSortField;
  direction: SortDirection;
  align?: "left" | "right";
  className?: string;
  onSort: (field: TaskSortField) => void;
}) {
  const active = activeField === field;
  let ariaSort: React.AriaAttributes["aria-sort"] = "none";
  if (active) ariaSort = direction === "asc" ? "ascending" : "descending";

  let actionLabel = `${label} 기준 내림차순 정렬`;
  if (active) actionLabel = `${label} 현재 ${direction === "asc" ? "오름차순" : "내림차순"}. 눌러서 정렬 방향 바꾸기`;

  return (
    <TableHead className={className} aria-sort={ariaSort}>
      <button
        type="button"
        className={cn(
          "group/sort inline-flex h-full w-full items-center gap-1 outline-none focus-visible:underline",
          align === "right" && "justify-end",
        )}
        aria-label={actionLabel}
        onClick={() => onSort(field)}
      >
        <span>{label}</span>
        {taskSortIcon(active, direction)}
      </button>
    </TableHead>
  );
}

function ConcurrencySettingsDialog() {
  const [open, setOpen] = React.useState(false);
  const [enabled, setEnabled] = React.useState(false);
  const [limit, setLimit] = React.useState("5");
  const [loading, setLoading] = React.useState(false);
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    setLoading(true);
    api
      .settings()
      .then((settings) => {
        setEnabled(!!settings.task_concurrency_enabled);
        setLimit(String(settings.task_concurrency_limit ?? 5));
      })
      .catch(() => {
        // Keep the dialog usable with defaults; reopening retries the request.
      })
      .finally(() => setLoading(false));
  }, [open]);

  async function save() {
    const nextLimit = Math.max(1, Math.floor(Number(limit) || 5));
    setSaving(true);
    try {
      await api.setSettings({ task_concurrency_enabled: enabled, task_concurrency_limit: nextLimit });
      toast.success(
        enabled
          ? `동시 실행 제한을 켰습니다. 작업을 최대 ${nextLimit}개까지 동시에 실행합니다.`
          : "작업 동시 실행 제한을 껐습니다.",
      );
      setOpen(false);
    } catch (error) {
      toast.error(`동시 실행 설정을 저장하지 못했습니다: ${(error as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline" className="ml-auto" aria-label="작업 동시 실행 설정">
          <SlidersHorizontalIcon /> 동시 실행 설정
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>작업 동시 실행 제한</DialogTitle>
          <DialogDescription>
            동시에 실행하는 작업 수를 제한합니다. 최대에 이르면 새 작업은 생성 순서대로 대기하다가 자리가 나면 자동으로
            시작합니다.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-5 py-2">
          <div className="flex items-center justify-between gap-4">
            <div className="grid gap-1">
              <Label htmlFor="task-concurrency-enabled">작업 동시 실행 제한 사용</Label>
              <span className="text-muted-foreground text-xs">기본값 꺼짐</span>
            </div>
            <Switch id="task-concurrency-enabled" checked={enabled} onCheckedChange={setEnabled} disabled={loading} />
          </div>
          {enabled && (
            <div className="grid gap-2">
              <Label htmlFor="task-concurrency-limit">동시 실행 최대 개수</Label>
              <Input
                id="task-concurrency-limit"
                type="number"
                min={1}
                className="w-32"
                value={limit}
                onChange={(event) => setLimit(event.target.value)}
                disabled={loading}
              />
            </div>
          )}
        </div>
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline">취소</Button>
          </DialogClose>
          <Button onClick={save} disabled={loading || saving}>
            {saving && <Loader2Icon className="animate-spin" />} 저장
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// TaskRow renders one row of the task table. Memoized so the per-second elapsed-time tick and
// the POLL_MS list refresh only re-render the rows whose data actually moved — a table page
// is 20 rows × (StatusBadge + Link + a Radix AlertDialog tree), far too heavy to rebuild
// wholesale on every parent render.
const TaskRow = React.memo(function TaskRow({
  task,
  nowSec,
  onDelete,
  onControl,
  onRename,
  onTogglePinned,
  onArchive,
  selected,
  onSelectedChange,
}: {
  task: Task;
  nowSec: number;
  onDelete: (id: string, options: DeleteTaskOptions) => Promise<void>;
  onControl: (id: string, action: "pause" | "resume") => Promise<void>;
  onRename: (task: Task, name: string) => Promise<void>;
  onTogglePinned: (task: Task) => Promise<void>;
  onArchive: (task: Task) => Promise<void>;
  selected: boolean;
  onSelectedChange: (id: string, checked: boolean) => void;
}) {
  return (
    <TableRow className="group border-border/60" data-state={selected ? "selected" : undefined}>
      <TableCell>
        <Checkbox
          checked={selected}
          onCheckedChange={(checked) => onSelectedChange(task.id, checked === true)}
          aria-label={`작업 ${task.id} 선택`}
        />
      </TableCell>
      <TableCell>
        <code className="bg-muted rounded px-1.5 py-0.5 font-mono text-xs">{task.id}</code>
      </TableCell>
      <TableCell className="font-medium">
        <div className="flex max-w-xs items-center gap-2">
          <TaskNameEditor task={task} onRename={onRename} />
          {task.active && <StarIcon className="size-4 shrink-0 fill-amber-400 text-amber-400" />}
          {taskIsPinned(task) && <PinIcon className="text-primary size-4 shrink-0" aria-label="상단 고정됨" />}
        </div>
      </TableCell>
      <TableCell className="text-muted-foreground max-w-40">
        <Link
          href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`}
          className="block truncate rounded-sm hover:text-foreground hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          title={task.description}
        >
          {task.description}
        </Link>
      </TableCell>
      <TableCell className="text-muted-foreground max-w-40 truncate" title={task.goal}>
        {task.goal}
      </TableCell>
      <TableCell>
        <StatusBadge domain="task" value={task.status} dot />
      </TableCell>
      <TableCell className="text-muted-foreground text-center text-xs tabular-nums">
        {typeof task.goals_total === "number" && task.goals_total > 0 ? (
          `${task.goals_met}/${task.goals_total}`
        ) : (
          <span className="text-muted-foreground">—</span>
        )}
      </TableCell>
      <TableCell className="text-center text-xs tabular-nums">
        {(() => {
          const f = task.findings;
          const total = f ? f.critical + f.high + f.medium + f.low : 0;
          if (!f || total === 0) return <span className="text-muted-foreground">0</span>;
          const seg = (n: number, cls: string) => <span className={n > 0 ? cls : "text-muted-foreground"}>{n}</span>;
          return (
            <span className="font-medium whitespace-nowrap" title="치명 / 높음 / 중간 / 낮음">
              {seg(f.critical, "text-rose-600 dark:text-rose-400")}
              <span className="text-muted-foreground">/</span>
              {seg(f.high, "text-red-600 dark:text-red-400")}
              <span className="text-muted-foreground">/</span>
              {seg(f.medium, "text-amber-600 dark:text-amber-400")}
              <span className="text-muted-foreground">/</span>
              {seg(f.low, "text-slate-600 dark:text-slate-400")}
            </span>
          );
        })()}
      </TableCell>
      <TableCell className="text-center text-xs tabular-nums">
        {task.in_flight && task.in_flight > 0 ? (
          <span className="text-foreground font-medium">{task.in_flight}</span>
        ) : (
          <span className="text-muted-foreground">0</span>
        )}
      </TableCell>
      <TableCell className="text-muted-foreground text-right text-xs whitespace-nowrap tabular-nums">
        {fmtDateTime(task.created_unix)}
      </TableCell>
      <TableCell className="text-right text-xs whitespace-nowrap tabular-nums">
        {(() => {
          const secs = taskDuration(task, nowSec);
          if (secs <= 0) return <span className="text-muted-foreground">—</span>;
          return (
            <span className={task.status === "running" ? "text-foreground" : "text-muted-foreground"}>
              {fmtDuration(secs)}
            </span>
          );
        })()}
      </TableCell>
      <TableCell
        className="text-right text-xs whitespace-nowrap tabular-nums"
        title={
          task.tokens
            ? `입력 ${task.tokens.input_tokens} · 캐시 ${task.tokens.cache_read_tokens} · 출력 ${task.tokens.output_tokens}`
            : undefined
        }
      >
        {task.tokens ? (
          <span className="text-muted-foreground">
            입력 <span className="text-foreground">{fmtTokens(task.tokens.input_tokens)}</span>
            {" · 캐시 "}
            <span className="text-foreground">{fmtTokens(task.tokens.cache_read_tokens)}</span>
            {" · 출력 "}
            <span className="text-foreground">{fmtTokens(task.tokens.output_tokens)}</span>
          </span>
        ) : (
          "—"
        )}
      </TableCell>
      <TableCell className="sticky right-0 z-10 bg-card text-right shadow-[-1px_0_0_0_hsl(var(--border))] group-hover:bg-muted/50">
        <div className="flex items-center justify-end gap-0.5">
          <Button size="icon" variant="ghost" asChild aria-label="작업 상세 보기" title="작업 상세 보기">
            <Link href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`}>
              <EyeIcon />
            </Link>
          </Button>
          <TaskControlButton task={task} onControl={onControl} />
          <TaskPinAction task={task} onTogglePinned={onTogglePinned} />
          <TaskArchiveAction task={task} onArchive={onArchive} />
          <DeleteTaskDialog task={task} onDelete={onDelete} />
        </div>
      </TableCell>
    </TableRow>
  );
});

function TaskNameEditor({ task, onRename }: { task: Task; onRename: (task: Task, name: string) => Promise<void> }) {
  const [editing, setEditing] = React.useState(false);
  const [name, setName] = React.useState(task.name ?? "");
  const [saving, setSaving] = React.useState(false);
  const inputRef = React.useRef<HTMLInputElement>(null);
  const cancelOnBlurRef = React.useRef(false);

  React.useEffect(() => {
    if (!editing) setName(task.name ?? "");
  }, [editing, task.name]);

  async function finishEditing() {
    if (cancelOnBlurRef.current) {
      cancelOnBlurRef.current = false;
      setName(task.name ?? "");
      setEditing(false);
      return;
    }

    const next = name.trim();
    if (!next || next === task.name?.trim()) {
      setName(task.name ?? "");
      setEditing(false);
      return;
    }

    setSaving(true);
    try {
      await onRename(task, next);
      setEditing(false);
    } catch {
      requestAnimationFrame(() => inputRef.current?.focus());
    } finally {
      setSaving(false);
    }
  }

  if (editing) {
    return (
      <div className="flex min-w-0 items-center gap-1">
        <Input
          ref={inputRef}
          value={name}
          maxLength={200}
          autoFocus
          disabled={saving}
          aria-label={`작업 #${task.id} 이름`}
          className="h-7 min-w-28 max-w-48 px-2 font-medium"
          onFocus={(event) => event.currentTarget.select()}
          onChange={(event) => setName(event.target.value)}
          onBlur={() => void finishEditing()}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.preventDefault();
              event.currentTarget.blur();
            } else if (event.key === "Escape") {
              event.preventDefault();
              cancelOnBlurRef.current = true;
              event.currentTarget.blur();
            }
          }}
        />
        {saving && <Spinner className="shrink-0" />}
      </div>
    );
  }

  return (
    <div className="flex min-w-0 items-center gap-1">
      <Link
        href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`}
        className="min-w-0 truncate rounded-sm hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        title={task.name?.trim() ? task.name : task.description}
      >
        {task.name?.trim() ? task.name : <span className="text-muted-foreground">이름 없음</span>}
      </Link>
      <Button
        type="button"
        variant="ghost"
        size="icon-xs"
        className="shrink-0 opacity-100 transition-opacity [@media(hover:hover)]:opacity-0 [@media(hover:hover)]:group-focus-within:opacity-100 [@media(hover:hover)]:group-hover:opacity-100"
        aria-label={`작업 #${task.id} 이름 바꾸기`}
        title="이름 바꾸기"
        onClick={() => {
          setName(task.name ?? "");
          setEditing(true);
        }}
      >
        <PencilIcon />
      </Button>
    </div>
  );
}

function taskPinIcon(pinning: boolean, pinned: boolean) {
  if (pinning) return <Spinner />;
  if (pinned) return <PinOffIcon />;
  return <PinIcon />;
}

function TaskPinAction({ task, onTogglePinned }: { task: Task; onTogglePinned: (task: Task) => Promise<void> }) {
  const [pinning, setPinning] = React.useState(false);
  const pinned = taskIsPinned(task);

  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      disabled={pinning}
      aria-label={pinned ? `작업 #${task.id} 상단 고정 해제` : `작업 #${task.id} 상단 고정`}
      title={pinned ? "상단 고정 해제" : "상단 고정"}
      onClick={async () => {
        setPinning(true);
        try {
          await onTogglePinned(task);
        } finally {
          setPinning(false);
        }
      }}
    >
      {taskPinIcon(pinning, pinned)}
    </Button>
  );
}

// 세 상태의 아이콘을 중첩 삼항식 대신 따로 쓴다. 저장소의 Biome 기준이 noNestedTernary를 금지한다.
function taskControlIcon(pending: boolean, action: "pause" | "resume" | null) {
  if (pending) return <Loader2Icon className="animate-spin" />;
  if (action === "resume") return <PlayIcon />;
  return <PauseIcon />;
}

// TaskControlButton은 행의 일시 중지/재개 버튼이다. 종료된 작업은 숨기지 않고 disabled로 렌더링해서
// 모든 행의 관리 열 너비가 같고 버튼 위치가 상태에 따라 움직이지 않게 한다.
function TaskControlButton({
  task,
  onControl,
}: {
  task: Task;
  onControl: (id: string, action: "pause" | "resume") => Promise<void>;
}) {
  const [pending, setPending] = React.useState(false);
  const action = taskControlAction(task.status);
  const label = action === "resume" ? "작업 재개" : "작업 일시 중지";
  return (
    <Button
      size="icon"
      variant="ghost"
      aria-label={label}
      title={action ? label : "이 상태에서는 일시 중지하거나 재개할 수 없습니다."}
      disabled={!action || pending}
      onClick={async () => {
        if (!action) return;
        setPending(true);
        try {
          await onControl(task.id, action);
        } finally {
          setPending(false);
        }
      }}
    >
      {taskControlIcon(pending, action)}
    </Button>
  );
}

function archiveBlockReason(task: Task): string {
  if (task.queued) return "대기 중인 작업은 먼저 일시 중지해야 합니다.";
  if (!ARCHIVABLE_STATUSES.has(task.status)) return "실행 중이거나 아직 끝나지 않은 작업은 먼저 일시 중지해야 합니다.";
  if (task.archive_blocked_by_task_id) {
    return `보관되지 않은 작업 #${task.archive_blocked_by_task_id}이(가) 이 작업을 직접 이어받았습니다. 그 작업을 먼저 보관하세요.`;
  }
  return "";
}

function ArchiveConfirmDialog({
  count,
  trigger,
  onConfirm,
}: {
  count: number;
  trigger: React.ReactNode;
  onConfirm: () => Promise<void>;
}) {
  const [open, setOpen] = React.useState(false);
  const [pending, setPending] = React.useState(false);
  return (
    <AlertDialog open={open} onOpenChange={(next) => !pending && setOpen(next)}>
      <AlertDialogTrigger asChild>{trigger}</AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{count === 1 ? "작업을 보관할까요?" : `작업 ${count}개를 보관할까요?`}</AlertDialogTitle>
          <AlertDialogDescription className="[overflow-wrap:anywhere]">
            보관하면 작업 실행 예약을 멈추고, 그래프, LLM 기록, 파일, 이 작업에만 속한 자산과 트래픽을 압축해 로컬 콜드
            스토리지로 옮깁니다. 보관이 끝나면 "보관됨" 탭에서 복원할 수 있습니다.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending}>취소</AlertDialogCancel>
          <AlertDialogAction
            disabled={pending}
            onClick={async (event) => {
              event.preventDefault();
              setPending(true);
              try {
                await onConfirm();
                setOpen(false);
              } finally {
                setPending(false);
              }
            }}
          >
            {pending && <Spinner data-icon="inline-start" />}
            보관
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function TaskArchiveAction({ task, onArchive }: { task: Task; onArchive: (task: Task) => Promise<void> }) {
  const reason = archiveBlockReason(task);
  const trigger = (
    <Button size="icon" variant="ghost" disabled={Boolean(reason)} aria-label={`작업 #${task.id} 보관`}>
      <ArchiveIcon />
    </Button>
  );
  if (reason) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex">{trigger}</span>
        </TooltipTrigger>
        <TooltipContent>{reason}</TooltipContent>
      </Tooltip>
    );
  }
  return <ArchiveConfirmDialog count={1} trigger={trigger} onConfirm={() => onArchive(task)} />;
}

const ARCHIVE_PROCESSING_STATES = new Set<TaskArchiveState>([
  "archive_queued",
  "archiving",
  "restore_queued",
  "restoring",
  "delete_queued",
  "deleting",
]);

const ARCHIVE_FAILED_STATES = new Set<TaskArchiveState>(["archive_failed", "restore_failed", "delete_failed"]);

function archiveStateLabel(state: TaskArchiveState): string {
  const labels: Record<TaskArchiveState, string> = {
    archive_queued: "보관 대기",
    archiving: "보관 중",
    archive_failed: "보관 실패",
    ready: "복원 가능",
    restore_queued: "복원 대기",
    restoring: "복원 중",
    restore_failed: "복원 실패",
    delete_queued: "삭제 대기",
    deleting: "삭제 중",
    delete_failed: "삭제 실패",
  };
  return labels[state];
}

function ArchiveStateBadge({ state }: { state: TaskArchiveState }) {
  let variant: "default" | "secondary" | "destructive" | "outline" = "outline";
  if (state === "ready") variant = "secondary";
  if (ARCHIVE_PROCESSING_STATES.has(state)) variant = "default";
  if (ARCHIVE_FAILED_STATES.has(state)) variant = "destructive";
  return <Badge variant={variant}>{archiveStateLabel(state)}</Badge>;
}

function formatArchiveBytes(bytes: number): string {
  if (bytes <= 0) return "—";
  if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
  if (bytes >= 1024 ** 2) return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${bytes} B`;
}

function archiveCompressionLabel(archive: TaskArchive): string {
  if (archive.original_size <= 0 || archive.compressed_size <= 0) return "—";
  const saved = Math.max(0, 100 - (archive.compressed_size / archive.original_size) * 100);
  return `${formatArchiveBytes(archive.compressed_size)} · ${saved.toFixed(0)}% 절약`;
}

function archiveDataTotal(archive: TaskArchive): number {
  return Object.values(archive.data_counts).reduce((sum, value) => sum + (Number(value) || 0), 0);
}

function archiveDate(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString();
}

function ArchiveDeleteDialog({
  archives,
  onConfirm,
  trigger,
}: {
  archives: TaskArchive[];
  onConfirm: () => Promise<void>;
  trigger: React.ReactNode;
}) {
  const [open, setOpen] = React.useState(false);
  const [pending, setPending] = React.useState(false);
  const rows = archives.reduce((sum, archive) => sum + archiveDataTotal(archive), 0);
  const bytes = archives.reduce((sum, archive) => sum + archive.compressed_size, 0);
  return (
    <AlertDialog open={open} onOpenChange={(next) => !pending && setOpen(next)}>
      <AlertDialogTrigger asChild>{trigger}</AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>작업 보관본 {archives.length}개를 영구 삭제할까요?</AlertDialogTitle>
          <AlertDialogDescription className="[overflow-wrap:anywhere]">
            약 {formatArchiveBytes(bytes)}의 보관 패키지와 관련 데이터 스냅숏 {rows.toLocaleString()}건을 영구
            삭제합니다. 되돌릴 수 없습니다.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending}>취소</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={pending}
            onClick={async (event) => {
              event.preventDefault();
              setPending(true);
              try {
                await onConfirm();
                setOpen(false);
              } finally {
                setPending(false);
              }
            }}
          >
            {pending && <Spinner data-icon="inline-start" />}
            영구 삭제
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function TaskArchivesPanel({ onChanged }: { onChanged: () => void }) {
  const [archives, setArchives] = React.useState<TaskArchive[]>([]);
  const [query, setQuery] = React.useState("");
  const [stateFilter, setStateFilter] = React.useState("all");
  const [page, setPage] = React.useState(1);
  const [pageSize, setPageSize] = React.useState(20);
  const [total, setTotal] = React.useState(0);
  const [selected, setSelected] = React.useState<Set<number>>(() => new Set());
  const [loading, setLoading] = React.useState(true);
  const pendingRestoreIDs = React.useRef(new Set<number>());

  const load = React.useCallback(async () => {
    try {
      const result = await api.taskArchives({
        page,
        size: pageSize,
        q: query.trim(),
        state: stateFilter === "all" ? undefined : stateFilter,
      });
      setArchives(result.items);
      setTotal(result.total);
      setSelected((current) => {
        const visible = new Set(result.items.map((archive) => archive.id));
        return new Set([...current].filter((id) => visible.has(id)));
      });
      const pending = [...pendingRestoreIDs.current];
      if (pending.length > 0) {
        const states = await Promise.allSettled(pending.map((id) => api.taskArchive(id)));
        let restored = false;
        states.forEach((state, index) => {
          if (state.status !== "rejected" || !(state.reason instanceof Error)) return;
          if (!state.reason.message.includes("보관본이 없습니다")) return;
          pendingRestoreIDs.current.delete(pending[index]);
          restored = true;
        });
        if (restored) onChanged();
      }
    } catch (error) {
      toast.error(`보관 목록을 읽지 못했습니다: ${(error as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, [onChanged, page, pageSize, query, stateFilter]);

  React.useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), 2_000);
    return () => clearInterval(timer);
  }, [load]);

  const selectable = React.useMemo(
    () => archives.filter((archive) => !ARCHIVE_PROCESSING_STATES.has(archive.state)),
    [archives],
  );
  const selectedArchives = React.useMemo(
    () => archives.filter((archive) => selected.has(archive.id)),
    [archives, selected],
  );
  const restorable = selectedArchives.filter(
    (archive) => archive.state === "ready" || archive.state === "restore_failed",
  );
  const deletable = selectedArchives.filter(
    (archive) => archive.state === "ready" || archive.state === "delete_failed",
  );
  const selectedAll = selectable.length > 0 && selectable.every((archive) => selected.has(archive.id));
  const selectedSome = selectable.some((archive) => selected.has(archive.id));

  const afterAction = React.useCallback(() => {
    setSelected(new Set());
    void load();
    onChanged();
  }, [load, onChanged]);

  async function restoreMany(items: TaskArchive[]) {
    try {
      const result =
        items.length === 1
          ? {
              items: [
                {
                  id: String(items[0].id),
                  ok: true,
                  queued: true,
                  archive_id: (await api.restoreTaskArchive(items[0].id)).id,
                },
              ],
            }
          : await api.restoreTaskArchives(items.map((archive) => archive.id));
      const succeeded = result.items.filter((item) => item.ok).length;
      const failed = result.items.length - succeeded;
      for (const item of result.items) {
        const archiveID = Number(item.archive_id ?? item.id);
        if (item.ok && Number.isSafeInteger(archiveID) && archiveID > 0) pendingRestoreIDs.current.add(archiveID);
      }
      if (succeeded > 0) toast.success(`작업 ${succeeded}개를 복원 대기열에 넣었습니다`);
      if (failed > 0) toast.error(`작업 ${failed}개를 복원하지 못했습니다`);
      afterAction();
    } catch (error) {
      toast.error(`작업을 복원하지 못했습니다: ${(error as Error).message}`);
    }
  }

  async function deleteMany(items: TaskArchive[]) {
    try {
      const result =
        items.length === 1
          ? {
              items: [
                {
                  id: String(items[0].id),
                  ok: true,
                  queued: true,
                  archive_id: (await api.deleteTaskArchive(items[0].id)).id,
                },
              ],
            }
          : await api.deleteTaskArchives(items.map((archive) => archive.id));
      const succeeded = result.items.filter((item) => item.ok).length;
      const failed = result.items.length - succeeded;
      if (succeeded > 0) toast.success(`보관본 ${succeeded}개를 영구 삭제 대기열에 넣었습니다`);
      if (failed > 0) toast.error(`보관본 ${failed}개를 삭제하지 못했습니다`);
      afterAction();
    } catch (error) {
      toast.error(`보관본을 영구 삭제하지 못했습니다: ${(error as Error).message}`);
      throw error;
    }
  }

  async function retry(archive: TaskArchive) {
    try {
      if (archive.state === "archive_failed") await api.archiveTask(String(archive.task_id));
      if (archive.state === "restore_failed") await api.restoreTaskArchive(archive.id);
      if (archive.state === "delete_failed") await api.deleteTaskArchive(archive.id);
      toast.success("처리 대기열에 다시 넣었습니다");
      afterAction();
    } catch (error) {
      toast.error(`보관본 처리를 재시도하지 못했습니다: ${(error as Error).message}`);
    }
  }

  return (
    <Card>
      <CardContent className="flex flex-col gap-4 px-0 pt-6">
        <div className="flex flex-wrap items-center gap-2 px-4 lg:px-6">
          <div className="relative w-full sm:max-w-xs">
            <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={query}
              onChange={(event) => {
                setQuery(event.target.value);
                setPage(1);
              }}
              placeholder="작업 이름 / 설명 / ID 검색"
              className="pl-8"
            />
          </div>
          <Select
            value={stateFilter}
            onValueChange={(value) => {
              setStateFilter(value);
              setPage(1);
            }}
          >
            <SelectTrigger className="w-36">
              <SelectValue placeholder="처리 상태" />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value="all">전체 상태</SelectItem>
                <SelectItem value="archive_queued">보관 대기</SelectItem>
                <SelectItem value="archiving">보관 중</SelectItem>
                <SelectItem value="ready">복원 가능</SelectItem>
                <SelectItem value="restore_queued">복원 대기</SelectItem>
                <SelectItem value="restoring">복원 중</SelectItem>
                <SelectItem value="delete_queued">삭제 대기</SelectItem>
                <SelectItem value="deleting">삭제 중</SelectItem>
                <SelectItem value="archive_failed">보관 실패</SelectItem>
                <SelectItem value="restore_failed">복원 실패</SelectItem>
                <SelectItem value="delete_failed">삭제 실패</SelectItem>
              </SelectGroup>
            </SelectContent>
          </Select>
          <span className="text-muted-foreground text-xs tabular-nums">보관본 {total}개</span>
          {selectedArchives.length > 0 && (
            <>
              <span className="text-xs tabular-nums">{selectedArchives.length}개 선택됨</span>
              {restorable.length > 0 && (
                <Button size="sm" variant="outline" onClick={() => void restoreMany(restorable)}>
                  <Undo2Icon data-icon="inline-start" />
                  복원 {restorable.length}
                </Button>
              )}
              {deletable.length > 0 && (
                <ArchiveDeleteDialog
                  archives={deletable}
                  onConfirm={() => deleteMany(deletable)}
                  trigger={
                    <Button size="sm" variant="destructive">
                      <Trash2Icon data-icon="inline-start" />
                      영구 삭제 {deletable.length}
                    </Button>
                  }
                />
              )}
            </>
          )}
        </div>
        {loading ? (
          <div className="flex justify-center py-20">
            <Spinner />
          </div>
        ) : archives.length === 0 ? (
          <Empty className="mx-4 border border-dashed lg:mx-6">
            <EmptyHeader>
              <EmptyTitle>작업 보관본이 없습니다</EmptyTitle>
              <EmptyDescription>일시 중지됐거나 종료된 작업은 현재 작업 목록에서 보관할 수 있습니다.</EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <Table className="**:data-[slot='table-cell']:px-4 **:data-[slot='table-head']:px-4">
            <TableHeader className="[&_tr]:border-t">
              <TableRow>
                <TableHead className="w-10">
                  <Checkbox
                    checked={selectedAll || (selectedSome ? "indeterminate" : false)}
                    onCheckedChange={(checked) =>
                      setSelected(checked === true ? new Set(selectable.map((archive) => archive.id)) : new Set())
                    }
                    aria-label="이 페이지의 보관본 모두 선택"
                  />
                </TableHead>
                <TableHead>작업</TableHead>
                <TableHead>원래 상태</TableHead>
                <TableHead>분류</TableHead>
                <TableHead>보관 시간</TableHead>
                <TableHead>압축 크기</TableHead>
                <TableHead>데이터 양</TableHead>
                <TableHead className="min-w-44">처리 상태</TableHead>
                <TableHead className="text-right">관리</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {archives.map((archive) => {
                const processing = ARCHIVE_PROCESSING_STATES.has(archive.state);
                const canRestore = archive.state === "ready" || archive.state === "restore_failed";
                const canDelete = archive.state === "ready" || archive.state === "delete_failed";
                return (
                  <TableRow key={archive.id} data-state={selected.has(archive.id) ? "selected" : undefined}>
                    <TableCell>
                      <Checkbox
                        checked={selected.has(archive.id)}
                        disabled={processing}
                        onCheckedChange={(checked) =>
                          setSelected((current) => {
                            const next = new Set(current);
                            if (checked === true) next.add(archive.id);
                            else next.delete(archive.id);
                            return next;
                          })
                        }
                        aria-label={`작업 #${archive.task_id} 보관본 선택`}
                      />
                    </TableCell>
                    <TableCell className="max-w-sm">
                      <div className="flex min-w-0 flex-col gap-0.5">
                        <span className="truncate font-medium">
                          {archive.task_name || archive.task_description || `작업 #${archive.task_id}`}
                        </span>
                        <span className="text-muted-foreground truncate text-xs">
                          #{archive.task_id} · {archive.task_description}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge domain="task" value={archive.original_status} dot />
                    </TableCell>
                    <TableCell className="text-muted-foreground">{archive.category_name || "미분류"}</TableCell>
                    <TableCell className="text-muted-foreground whitespace-nowrap text-xs">
                      {archiveDate(archive.archived_at || archive.requested_at)}
                    </TableCell>
                    <TableCell
                      className="whitespace-nowrap text-xs"
                      title={`압축 전 ${formatArchiveBytes(archive.original_size)}`}
                    >
                      {archiveCompressionLabel(archive)}
                    </TableCell>
                    <TableCell className="text-xs tabular-nums">{archiveDataTotal(archive).toLocaleString()}</TableCell>
                    <TableCell>
                      <div className="flex min-w-0 flex-col gap-1.5">
                        <ArchiveStateBadge state={archive.state} />
                        {processing && <Progress value={archive.progress} />}
                        {archive.error && (
                          <p className="text-destructive text-xs [overflow-wrap:anywhere]">{archive.error}</p>
                        )}
                        {[...new Set(archive.warnings ?? [])].map((warning) => (
                          <p
                            key={warning}
                            className="text-amber-700 text-xs [overflow-wrap:anywhere] dark:text-amber-400"
                          >
                            {warning}
                          </p>
                        ))}
                        <span className="text-muted-foreground text-xs">
                          {archive.phase} · {archive.progress}%
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-0.5">
                        {ARCHIVE_FAILED_STATES.has(archive.state) && (
                          <Button
                            size="icon"
                            variant="ghost"
                            onClick={() => void retry(archive)}
                            aria-label="보관 처리 재시도"
                            title="재시도"
                          >
                            <Undo2Icon />
                          </Button>
                        )}
                        {canRestore && (
                          <Button
                            size="icon"
                            variant="ghost"
                            onClick={() => void restoreMany([archive])}
                            aria-label="작업 복원"
                            title="작업 복원"
                          >
                            <Undo2Icon />
                          </Button>
                        )}
                        {canDelete && (
                          <ArchiveDeleteDialog
                            archives={[archive]}
                            onConfirm={() => deleteMany([archive])}
                            trigger={
                              <Button size="icon" variant="ghost" aria-label="보관본 영구 삭제" title="영구 삭제">
                                <Trash2Icon />
                              </Button>
                            }
                          />
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
        <TablePagination
          page={page}
          pageSize={pageSize}
          total={total}
          onPageChange={setPage}
          onPageSizeChange={(size) => {
            setPageSize(size);
            setPage(1);
          }}
        />
      </CardContent>
    </Card>
  );
}

const emptyDeleteOptions = (): DeleteTaskOptions => ({
  delete_assets: false,
  delete_traffic: false,
  delete_files: false,
  delete_findings: false,
  delete_llm_records: false,
});

const deleteOptionKeys: (keyof DeleteTaskOptions)[] = [
  "delete_assets",
  "delete_traffic",
  "delete_files",
  "delete_findings",
  "delete_llm_records",
];

// DeleteOptionFields renders the "also clean up related data" checkbox block shared by the
// single-task and bulk delete dialogs. idPrefix keeps the label/input ids unique when
// several dialogs live in the same table.
function DeleteOptionFields({
  idPrefix,
  options,
  onOptionsChange,
  disabled,
}: {
  idPrefix: string;
  options: DeleteTaskOptions;
  onOptionsChange: React.Dispatch<React.SetStateAction<DeleteTaskOptions>>;
  disabled: boolean;
}) {
  const selectedCount = deleteOptionKeys.filter((key) => options[key]).length;
  let allChecked: boolean | "indeterminate" = false;
  if (selectedCount === deleteOptionKeys.length) {
    allChecked = true;
  } else if (selectedCount > 0) {
    allChecked = "indeterminate";
  }

  const updateOption = (key: keyof DeleteTaskOptions, checked: boolean) => {
    onOptionsChange((current) => ({ ...current, [key]: checked }));
  };

  const updateAllOptions = (checked: boolean) => {
    onOptionsChange({
      delete_assets: checked,
      delete_traffic: checked,
      delete_files: checked,
      delete_findings: checked,
      delete_llm_records: checked,
    });
  };

  return (
    <FieldSet disabled={disabled}>
      <FieldLegend variant="label">관련 데이터도 정리</FieldLegend>
      <FieldGroup className="gap-3">
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-all-${idPrefix}`}
            checked={allChecked}
            onCheckedChange={(checked) => updateAllOptions(checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-all-${idPrefix}`}>전체 삭제</FieldLabel>
            <FieldDescription>
              아래의 관련 데이터를 모두 선택합니다. 자산, 트래픽, 파일, 취약점, LLM 요청/응답 기록이 포함됩니다.
            </FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-assets-${idPrefix}`}
            checked={options.delete_assets}
            onCheckedChange={(checked) => updateOption("delete_assets", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-assets-${idPrefix}`}>관련 자산</FieldLabel>
            <FieldDescription>
              이 작업에만 속한 자산을 삭제합니다. 공유 자산은 이 작업과의 연결만 해제합니다.
            </FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-traffic-${idPrefix}`}
            checked={options.delete_traffic}
            onCheckedChange={(checked) => updateOption("delete_traffic", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-traffic-${idPrefix}`}>관련 트래픽</FieldLabel>
            <FieldDescription>
              관련 자산의 호스트 이름과 정확히 일치하는 트래픽을 삭제합니다. 다른 작업이 아직 참조하는 공유 호스트의
              트래픽은 유지합니다.
            </FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-files-${idPrefix}`}
            checked={options.delete_files}
            onCheckedChange={(checked) => updateOption("delete_files", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-files-${idPrefix}`}>작업 파일</FieldLabel>
            <FieldDescription>
              이 작업의 작업 디렉터리에 있는 업로드 파일, 명령 출력, 그 밖의 결과물을 삭제합니다.
            </FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-findings-${idPrefix}`}
            checked={options.delete_findings}
            onCheckedChange={(checked) => updateOption("delete_findings", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-findings-${idPrefix}`}>관련 취약점</FieldLabel>
            <FieldDescription>이 작업이 만든 취약점 기록과 취약점 보고서를 영구 삭제합니다.</FieldDescription>
          </FieldContent>
        </Field>
        <Field orientation="horizontal">
          <Checkbox
            id={`delete-llm-records-${idPrefix}`}
            checked={options.delete_llm_records}
            onCheckedChange={(checked) => updateOption("delete_llm_records", checked === true)}
          />
          <FieldContent>
            <FieldLabel htmlFor={`delete-llm-records-${idPrefix}`}>LLM 요청/응답 기록</FieldLabel>
            <FieldDescription>이 작업이 기록한 LLM 요청, 응답, 토큰, 오류 상세를 영구 삭제합니다.</FieldDescription>
          </FieldContent>
        </Field>
      </FieldGroup>
    </FieldSet>
  );
}

function DeleteTaskDialog({
  task,
  onDelete,
}: {
  task: Task;
  onDelete: (id: string, options: DeleteTaskOptions) => Promise<void>;
}) {
  const [open, setOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [options, setOptions] = React.useState<DeleteTaskOptions>(emptyDeleteOptions);

  const handleOpenChange = (next: boolean) => {
    if (deleting) return;
    setOpen(next);
    if (next) setOptions(emptyDeleteOptions());
  };

  const handleDelete = async () => {
    setDeleting(true);
    try {
      await onDelete(task.id, options);
      setOpen(false);
    } finally {
      setDeleting(false);
    }
  };

  return (
    <AlertDialog open={open} onOpenChange={handleOpenChange}>
      <AlertDialogTrigger asChild>
        <Button size="icon" variant="outline" aria-label="작업 삭제">
          <Trash2Icon className="text-destructive" />
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>작업 #{task.id}을 삭제할까요?</AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {task.description ? (
              <>
                "
                <span className="break-all">
                  {task.description.length > 80 ? `${task.description.slice(0, 80)}…` : task.description}
                </span>
                "
              </>
            ) : (
              "이 작업"
            )}
            의 실행 기록과 탐색 경로를 영구 삭제합니다.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <DeleteOptionFields idPrefix={task.id} options={options} onOptionsChange={setOptions} disabled={deleting} />
        <AlertDialogFooter>
          <AlertDialogCancel disabled={deleting}>취소</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={deleting}
            onClick={(event) => {
              event.preventDefault();
              void handleDelete();
            }}
          >
            {deleting && <Spinner data-icon="inline-start" />}
            {deleting ? "삭제 중" : "삭제"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

// BulkDeleteTasksDialog deletes every checked task with one shared set of cleanup options.
// The backend has no batch endpoint, so deletion runs one task at a time (see deleteTasks)
// and the button shows live progress.
// MoveTasksCategoryDialog confirms a target before applying, so a mis-click on a
// large selection cannot silently re-file every task. UNCATEGORIZED_VALUE stands
// in for "no category" because Select rejects an empty string value.
function MoveTasksCategoryDialog({
  categories,
  count,
  moving,
  onMove,
}: {
  categories: TaskCategory[];
  count: number;
  moving: boolean;
  onMove: (categoryID?: number) => Promise<void>;
}) {
  const [open, setOpen] = React.useState(false);
  const [target, setTarget] = React.useState(UNCATEGORIZED_VALUE);

  const handleOpenChange = (next: boolean) => {
    if (moving) return;
    setOpen(next);
    if (next) setTarget(UNCATEGORIZED_VALUE);
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          <FolderInputIcon data-icon="inline-start" /> 분류 바꾸기
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>선택한 작업 {count}개의 분류 바꾸기</DialogTitle>
          <DialogDescription>
            선택한 작업 모두에 대상 분류를 적용합니다. "미분류"를 고르면 현재 분류에서 빼냅니다.
          </DialogDescription>
        </DialogHeader>
        <Field>
          <FieldLabel htmlFor="bulk-category">대상 분류</FieldLabel>
          <Select value={target} onValueChange={setTarget} disabled={moving}>
            <SelectTrigger id="bulk-category" className="w-full">
              <SelectValue placeholder="분류 선택" />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value={UNCATEGORIZED_VALUE}>미분류</SelectItem>
                {categories.map((category) => (
                  <SelectItem key={category.id} value={String(category.id)}>
                    {category.name}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          {categories.length === 0 && (
            <FieldDescription>분류가 아직 없습니다. "분류 관리"에서 먼저 만드세요.</FieldDescription>
          )}
        </Field>
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline" disabled={moving}>
              취소
            </Button>
          </DialogClose>
          <Button
            disabled={moving}
            onClick={() => {
              void onMove(target === UNCATEGORIZED_VALUE ? undefined : Number(target)).then(() => setOpen(false));
            }}
          >
            {moving && <Spinner data-icon="inline-start" />}
            이동
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function BulkDeleteTasksDialog({
  ids,
  onDelete,
}: {
  ids: string[];
  onDelete: (ids: string[], options: DeleteTaskOptions, onProgress: (done: number) => void) => Promise<void>;
}) {
  const [open, setOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [done, setDone] = React.useState(0);
  const [options, setOptions] = React.useState<DeleteTaskOptions>(emptyDeleteOptions);

  const handleOpenChange = (next: boolean) => {
    if (deleting) return;
    setOpen(next);
    if (next) {
      setOptions(emptyDeleteOptions());
      setDone(0);
    }
  };

  const handleDelete = async () => {
    setDeleting(true);
    setDone(0);
    try {
      await onDelete(ids, options, setDone);
      setOpen(false);
    } finally {
      setDeleting(false);
    }
  };

  return (
    <AlertDialog open={open} onOpenChange={handleOpenChange}>
      <AlertDialogTrigger asChild>
        <Button size="sm" variant="outline">
          <Trash2Icon className="text-destructive" /> 선택 삭제 {ids.length}
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent className="max-h-[85vh] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>선택한 작업 {ids.length}개를 삭제할까요?</AlertDialogTitle>
          <AlertDialogDescription>
            이 작업들의 실행 기록과 탐색 경로를 영구 삭제합니다. 아래 정리 옵션은 선택한 작업 모두에 적용됩니다.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <div className="text-muted-foreground flex flex-wrap gap-1 text-xs">
          {ids.slice(0, 30).map((id) => (
            <code key={id} className="bg-muted rounded px-1.5 py-0.5 font-mono">
              #{id}
            </code>
          ))}
          {ids.length > 30 && <span className="self-center">…총 {ids.length}개</span>}
        </div>
        <DeleteOptionFields idPrefix="bulk" options={options} onOptionsChange={setOptions} disabled={deleting} />
        <AlertDialogFooter>
          <AlertDialogCancel disabled={deleting}>취소</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={deleting}
            onClick={(event) => {
              event.preventDefault();
              void handleDelete();
            }}
          >
            {deleting && <Spinner data-icon="inline-start" />}
            {deleting ? `삭제 중 ${done}/${ids.length}` : `작업 ${ids.length}개 삭제`}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

// CreateTaskSheet is the new-task drawer. Its form state lives HERE, not in TasksPage: with
// description/goal held by the page component every keystroke re-rendered the whole task table
// behind the drawer (plus its sticky column and 20 AlertDialog trees), which showed up as
// input lag. Now typing only re-renders the drawer.
function SourceTaskPicker({
  tasks,
  value,
  onValueChange,
  portalContainer,
}: {
  tasks: Task[];
  value: string[];
  onValueChange: (value: string[]) => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const tasksByID = React.useMemo(() => new Map(tasks.map((task) => [task.id, task])), [tasks]);
  const taskIDs = React.useMemo(() => tasks.map((task) => task.id), [tasks]);
  const atLimit = value.length >= MAX_SOURCE_TASKS;

  const handleValueChange = (next: string[]) => {
    onValueChange(next.slice(0, MAX_SOURCE_TASKS));
  };

  return (
    <Combobox
      items={taskIDs}
      itemToStringValue={(taskID) => {
        const task = tasksByID.get(taskID);
        return task ? `${task.id} ${task.description} ${task.goal}` : taskID;
      }}
      multiple
      value={value}
      onValueChange={handleValueChange}
    >
      <ComboboxChips>
        <ComboboxValue>
          {value.map((taskID) => (
            <ComboboxChip key={taskID}>#{taskID}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput
          id="source-tasks"
          placeholder={
            atLimit ? `작업은 최대 ${MAX_SOURCE_TASKS}개까지 연결할 수 있습니다` : "작업 ID, 설명, 목표 검색"
          }
          disabled={atLimit}
        />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxEmpty>일치하는 작업이 없습니다</ComboboxEmpty>
        <ComboboxList>
          {(taskID) => {
            const task = tasksByID.get(taskID);
            return (
              <ComboboxItem key={taskID} value={taskID} disabled={atLimit && !value.includes(taskID)}>
                <div className="flex min-w-0 flex-1 items-center gap-2">
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm font-medium">
                      #{taskID} · {task?.description ?? "알 수 없는 작업"}
                    </p>
                    {task?.goal && <p className="text-muted-foreground truncate text-xs">{task.goal}</p>}
                  </div>
                  {task && <StatusBadge domain="task" value={task.status} />}
                </div>
              </ComboboxItem>
            );
          }}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  );
}

// CategoryPicker는 새 작업 양식의 단일 선택 분류 선택기다. 기존 분류를 검색할 수 있고, 없는 이름을 입력한 뒤
// Enter를 누르면(또는 드롭다운의 "만들기"를 누르면) 바로 분류를 만들어 선택한다. 선택한 항목은 지울 수 있는 태그로 보인다.
// 분류는 전역 자원이라 여기서 만드는 것은 "분류 관리"에서 직접 만드는 것과 같다. 분류는 하나만 고를 수 있다.
function CategoryPicker({
  categories,
  value,
  onValueChange,
  onCategoryCreated,
  portalContainer,
}: {
  categories: TaskCategory[];
  value?: number;
  onValueChange: (categoryID?: number) => void;
  onCategoryCreated: () => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const [inputValue, setInputValue] = React.useState("");
  const [creating, setCreating] = React.useState(false);
  // 새로 만든 분류는 부모가 다시 가져와야 categories에 들어오므로 로컬에 한 벌 남겨 둔다.
  // 그래야 그 사이에 선택된 칩과 드롭다운이 "알 수 없는 분류"로 보이지 않는다.
  const [localExtra, setLocalExtra] = React.useState<TaskCategory[]>([]);

  const allCategories = React.useMemo(() => {
    const byID = new Map<number, TaskCategory>();
    for (const category of categories) byID.set(category.id, category);
    for (const category of localExtra) if (!byID.has(category.id)) byID.set(category.id, category);
    return [...byID.values()];
  }, [categories, localExtra]);

  const byID = React.useMemo(() => new Map(allCategories.map((c) => [String(c.id), c])), [allCategories]);
  const categoryIDs = React.useMemo(() => allCategories.map((c) => String(c.id)), [allCategories]);
  const selectedIDs = value != null ? [String(value)] : [];

  const trimmed = inputValue.trim();
  const lower = trimmed.toLowerCase();
  // base-ui 기본 부분 문자열 필터와 같은 방식으로 "관련 분류가 있는지"를 판단한다.
  const matchCount = trimmed
    ? allCategories.filter((c) => c.name.toLowerCase().includes(lower)).length
    : allCategories.length;

  const createAndSelect = async () => {
    if (!trimmed || creating) return;
    // 이름이 정확히 일치하는 분류가 이미 있으면 새로 만들지 않고 그것을 선택한다.
    const existing = allCategories.find((c) => c.name.toLowerCase() === lower);
    if (existing) {
      onValueChange(existing.id);
      setInputValue("");
      return;
    }
    setCreating(true);
    try {
      const created = await api.createTaskCategory(trimmed);
      setLocalExtra((prev) => [...prev, created]);
      onValueChange(created.id);
      setInputValue("");
      onCategoryCreated();
      toast.success(`분류 "${created.name}"을(를) 만들었습니다`);
    } catch (e) {
      toast.error(`분류를 만들지 못했습니다: ${(e as Error).message}`);
    } finally {
      setCreating(false);
    }
  };

  return (
    <Combobox
      items={categoryIDs}
      itemToStringValue={(id) => byID.get(id)?.name ?? id}
      multiple
      value={selectedIDs}
      onValueChange={(next: string[]) => {
        // 단일 선택: 가장 최근에 고른 하나를 쓴다. 칩을 지우면(비우면) 미분류로 돌아간다.
        const last = next[next.length - 1];
        onValueChange(last ? Number(last) : undefined);
        setInputValue("");
      }}
      inputValue={inputValue}
      onInputValueChange={setInputValue}
    >
      <ComboboxChips>
        <ComboboxValue>
          {selectedIDs.map((id) => (
            <ComboboxChip key={id}>{byID.get(id)?.name ?? "알 수 없는 분류"}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput
          id="task-category"
          placeholder={selectedIDs.length ? "" : "분류 검색 또는 새 이름 입력 후 Enter로 만들기"}
          onKeyDown={(e) => {
            // 일치하는 항목이 전혀 없으면 Enter = 만들기. 일치 항목이 있으면 base-ui의 "Enter로 강조 항목 선택"을 유지한다.
            if (e.key === "Enter" && matchCount === 0 && trimmed) {
              e.preventDefault();
              void createAndSelect();
            }
          }}
        />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxList>
          {(id: string) => (
            <ComboboxItem key={id} value={id}>
              {byID.get(id)?.name ?? id}
            </ComboboxItem>
          )}
        </ComboboxList>
        {matchCount === 0 &&
          (trimmed ? (
            <button
              type="button"
              disabled={creating}
              onClick={() => void createAndSelect()}
              className="flex w-full items-center gap-2 px-2 py-2 text-left text-sm hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
            >
              {creating ? <Spinner className="size-4" /> : <PlusIcon className="size-4" />}
              분류 "{trimmed}" 만들기
            </button>
          ) : (
            <div className="px-2 py-2 text-sm text-muted-foreground">이름을 입력해 분류를 검색하거나 만드세요</div>
          ))}
      </ComboboxContent>
    </Combobox>
  );
}

const COMPANY_SCOPE_LABELS: Record<string, string> = {
  domain: "도메인",
  ip: "IP",
  cidr: "CIDR",
  icp: "ICP",
  keyword: "키워드",
};

function companyScopeSummary(company: Company): string {
  const rows = company.scope ?? [];
  if (rows.length === 0) return "자산 범위 설정 안 됨";
  const preview = rows.slice(0, 3).map((row) => {
    const value = row.raw || row.value || row.domain || row.net || "";
    return `${COMPANY_SCOPE_LABELS[row.kind] ?? row.kind}: ${value}`;
  });
  return `${preview.join(" · ")}${rows.length > preview.length ? ` · 그 밖에 ${rows.length - preview.length}건` : ""}`;
}

function CompanyPicker({
  companies,
  value,
  onValueChange,
  portalContainer,
}: {
  companies: Company[];
  value: number[];
  onValueChange: (value: number[]) => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const companiesByID = React.useMemo(
    () => new Map(companies.map((company) => [String(company.id), company])),
    [companies],
  );
  const companyIDs = React.useMemo(() => companies.map((company) => String(company.id)), [companies]);
  const selectedIDs = React.useMemo(() => value.map(String), [value]);

  return (
    <Combobox
      items={companyIDs}
      itemToStringValue={(companyID) => {
        const company = companiesByID.get(companyID);
        return company ? `${company.name} ${companyScopeSummary(company)}` : companyID;
      }}
      multiple
      value={selectedIDs}
      onValueChange={(next) => onValueChange(next.map(Number).filter(Number.isFinite))}
    >
      <ComboboxChips>
        <ComboboxValue>
          {selectedIDs.map((companyID) => (
            <ComboboxChip key={companyID}>{companiesByID.get(companyID)?.name ?? `기업 #${companyID}`}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput id="task-companies" placeholder="기업 이름 또는 자산 범위 검색" />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxEmpty>일치하는 기업이 없습니다</ComboboxEmpty>
        <ComboboxList>
          {(companyID) => {
            const company = companiesByID.get(companyID);
            return (
              <ComboboxItem key={companyID} value={companyID}>
                <div className="flex min-w-0 flex-1 flex-col gap-0.5">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="min-w-0 flex-1 truncate font-medium">{company?.name ?? `기업 #${companyID}`}</span>
                    <span className="text-muted-foreground shrink-0 text-xs tabular-nums">
                      자산 {company?.asset_count ?? 0}개
                    </span>
                  </div>
                  {company && (
                    <span className="text-muted-foreground truncate text-xs" title={companyScopeSummary(company)}>
                      {companyScopeSummary(company)}
                    </span>
                  )}
                </div>
              </ComboboxItem>
            );
          }}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  );
}

type CategoryManagementView = number | "uncategorized" | "new";

function CategoryDropTarget({
  value,
  name,
  count,
  selected,
  disabled,
  onSelect,
}: {
  value: string;
  name: string;
  count: number;
  selected: boolean;
  disabled: boolean;
  onSelect: () => void;
}) {
  const { isOver, setNodeRef } = useDroppable({ id: `category:${value}`, disabled });

  return (
    <button
      ref={setNodeRef}
      type="button"
      className={cn(
        "min-w-0 rounded-md border border-transparent px-2.5 py-2 text-left transition-colors",
        selected ? "bg-accent text-accent-foreground" : "hover:bg-accent/50",
        isOver && "border-primary bg-primary/10 text-foreground",
      )}
      onClick={onSelect}
    >
      <span className="block truncate font-medium text-sm">{name}</span>
      <span className="block truncate text-muted-foreground text-xs">{isOver ? "놓아서 이동" : `작업 ${count}개`}</span>
    </button>
  );
}

function DraggableCategoryTask({ task, disabled, moving }: { task: Task; disabled: boolean; moving: boolean }) {
  const { attributes, isDragging, listeners, setNodeRef } = useDraggable({
    id: `task:${task.id}`,
    disabled,
  });

  return (
    <Item ref={setNodeRef} variant="outline" size="sm" className={cn(isDragging && "opacity-40")}>
      <ItemMedia className="group-has-data-[slot=item-description]/item:self-center group-has-data-[slot=item-description]/item:translate-y-0">
        {moving ? (
          <Spinner />
        ) : (
          <Button
            type="button"
            size="icon-xs"
            variant="ghost"
            className="touch-none cursor-grab active:cursor-grabbing"
            disabled={disabled}
            {...listeners}
            {...attributes}
            aria-label={`작업 #${task.id} 끌기`}
            title="작업 끌기"
          >
            <GripVerticalIcon />
          </Button>
        )}
      </ItemMedia>
      <ItemContent className="min-w-0">
        <ItemTitle className="w-full min-w-0">
          <Link href={`/function/tasks/detail?id=${encodeURIComponent(task.id)}`} className="truncate hover:underline">
            {task.name?.trim() || task.description || `작업 #${task.id}`}
          </Link>
        </ItemTitle>
        <ItemDescription className="line-clamp-1">
          #{task.id} · {task.description}
        </ItemDescription>
      </ItemContent>
      <ItemActions>
        <StatusBadge domain="task" value={task.status} />
      </ItemActions>
    </Item>
  );
}

function CategoryTaskDragPreview({ task }: { task: Task }) {
  return (
    <Item variant="outline" size="sm" className="w-80 bg-background shadow-lg">
      <ItemMedia className="group-has-data-[slot=item-description]/item:self-center group-has-data-[slot=item-description]/item:translate-y-0">
        <GripVerticalIcon className="size-4 text-muted-foreground" />
      </ItemMedia>
      <ItemContent className="min-w-0">
        <ItemTitle className="w-full min-w-0 truncate">
          {task.name?.trim() || task.description || `작업 #${task.id}`}
        </ItemTitle>
        <ItemDescription className="line-clamp-1">#{task.id}</ItemDescription>
      </ItemContent>
    </Item>
  );
}

function CategoryManagementSheet({
  categories,
  tasks,
  onChanged,
  onTaskMoved,
}: {
  categories: TaskCategory[];
  tasks: Task[];
  onChanged: () => void;
  onTaskMoved: (taskID: string, category: TaskCategory | null) => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [selectedView, setSelectedView] = React.useState<CategoryManagementView>("new");
  const [draftName, setDraftName] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [deleteOpen, setDeleteOpen] = React.useState(false);
  const [deleting, setDeleting] = React.useState(false);
  const [movingTaskID, setMovingTaskID] = React.useState<string | null>(null);
  const [activeTaskID, setActiveTaskID] = React.useState<string | null>(null);
  const wasOpen = React.useRef(false);
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(KeyboardSensor),
  );

  React.useEffect(() => {
    if (open && !wasOpen.current) {
      const first = categories[0];
      if (first) {
        setSelectedView(first.id);
        setDraftName(first.name);
      } else if (tasks.some((task) => task.category_id == null)) {
        setSelectedView("uncategorized");
        setDraftName("");
      } else {
        setSelectedView("new");
        setDraftName("");
      }
    }
    wasOpen.current = open;
  }, [categories, open, tasks]);

  const selectedCategory = React.useMemo(
    () =>
      typeof selectedView === "number" ? (categories.find((category) => category.id === selectedView) ?? null) : null,
    [categories, selectedView],
  );

  const uncategorizedCount = React.useMemo(() => tasks.filter((task) => task.category_id == null).length, [tasks]);

  const visibleTasks = React.useMemo(() => {
    if (selectedView === "uncategorized") return tasks.filter((task) => task.category_id == null);
    if (typeof selectedView === "number") return tasks.filter((task) => task.category_id === selectedView);
    return [];
  }, [selectedView, tasks]);

  const activeTask = React.useMemo(() => tasks.find((task) => task.id === activeTaskID) ?? null, [activeTaskID, tasks]);

  const selectCategory = (category: TaskCategory) => {
    setSelectedView(category.id);
    setDraftName(category.name);
  };

  const selectUncategorized = () => {
    setSelectedView("uncategorized");
    setDraftName("");
  };

  const startNew = () => {
    setSelectedView("new");
    setDraftName("");
  };

  async function saveCategory() {
    const name = draftName.trim();
    if (!name || saving || selectedView === "uncategorized") return;
    setSaving(true);
    try {
      if (selectedView === "new") {
        const created = await api.createTaskCategory(name);
        setSelectedView(created.id);
        setDraftName(created.name);
        toast.success("분류를 만들었습니다");
      } else {
        const updated = await api.renameTaskCategory(selectedView, name);
        setDraftName(updated.name);
        toast.success("분류를 수정했습니다");
      }
      onChanged();
    } catch (error) {
      toast.error(`분류를 ${selectedView === "new" ? "만들" : "수정하"}지 못했습니다: ${(error as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  async function deleteCategory() {
    if (!selectedCategory || deleting) return;
    const deletedID = selectedCategory.id;
    setDeleting(true);
    try {
      await api.deleteTaskCategory(deletedID);
      const next = categories.find((category) => category.id !== deletedID);
      if (next) selectCategory(next);
      else selectUncategorized();
      toast.success("분류를 삭제했습니다. 관련 작업은 미분류로 옮겼습니다.");
      setDeleteOpen(false);
      onChanged();
    } catch (error) {
      toast.error(`분류를 삭제하지 못했습니다: ${(error as Error).message}`);
    } finally {
      setDeleting(false);
    }
  }

  async function moveTask(task: Task, destination: string) {
    if (movingTaskID) return;
    const category =
      destination === "uncategorized" ? null : (categories.find((item) => item.id === Number(destination)) ?? null);
    if (destination !== "uncategorized" && !category) {
      toast.error("대상 분류가 없습니다. 새로 고친 뒤 다시 시도하세요.");
      return;
    }
    if (task.category_id === category?.id || (task.category_id == null && category == null)) return;

    setMovingTaskID(task.id);
    try {
      await api.updateTaskCategory(task.id, category?.id);
      onTaskMoved(task.id, category);
      toast.success(`작업 #${task.id}을 "${category?.name ?? "미분류"}"(으)로 옮겼습니다`);
    } catch (error) {
      toast.error(`작업을 옮기지 못했습니다: ${(error as Error).message}`);
    } finally {
      setMovingTaskID(null);
    }
  }

  function handleDragStart(event: DragStartEvent) {
    const id = String(event.active.id);
    setActiveTaskID(id.startsWith("task:") ? id.slice("task:".length) : null);
  }

  function handleDragEnd(event: DragEndEvent) {
    const activeID = String(event.active.id);
    const taskID = activeID.startsWith("task:") ? activeID.slice("task:".length) : null;
    setActiveTaskID(null);
    if (!taskID || !event.over) return;
    const destination = String(event.over.id);
    if (!destination.startsWith("category:")) return;
    const task = tasks.find((item) => item.id === taskID);
    if (!task) return;
    void moveTask(task, destination.slice("category:".length));
  }

  const saveLabel = saving ? "저장 중" : selectedView === "new" ? "분류 만들기" : "변경 사항 저장";

  return (
    <>
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetTrigger asChild>
          <Button size="sm" variant="outline">
            <TagsIcon data-icon="inline-start" />
            분류 관리
          </Button>
        </SheetTrigger>
        <SheetContent className="grid h-full w-full! max-w-none! grid-rows-[auto_minmax(0,1fr)_auto] gap-0 overflow-hidden p-0 sm:w-[48rem]! sm:max-w-[48rem]!">
          <SheetHeader className="border-b px-6 py-5">
            <SheetTitle>작업 분류 관리</SheetTitle>
            <SheetDescription>
              분류는 작업 필터와 보관에 씁니다. 바꿔도 작업 실행에는 영향이 없고, 삭제하면 작업은 미분류로 옮겨집니다.
            </SheetDescription>
          </SheetHeader>
          <DndContext
            sensors={sensors}
            onDragStart={handleDragStart}
            onDragCancel={() => setActiveTaskID(null)}
            onDragEnd={handleDragEnd}
          >
            <div className="grid min-h-0 overflow-y-auto lg:grid-cols-[15rem_minmax(0,1fr)] lg:overflow-hidden">
              <div className="flex min-h-0 flex-col border-b p-3 lg:border-r lg:border-b-0">
                <Button type="button" variant="outline" className="w-full" onClick={startNew}>
                  <PlusIcon data-icon="inline-start" />새 분류
                </Button>
                <ScrollArea className="mt-2 max-h-44 lg:max-h-none lg:flex-1">
                  <div className="flex flex-col gap-1 pr-2">
                    <CategoryDropTarget
                      value="uncategorized"
                      name="미분류"
                      count={uncategorizedCount}
                      selected={selectedView === "uncategorized"}
                      disabled={movingTaskID != null}
                      onSelect={selectUncategorized}
                    />
                    {categories.map((category) => (
                      <CategoryDropTarget
                        key={category.id}
                        value={String(category.id)}
                        name={category.name}
                        count={category.task_count}
                        selected={selectedView === category.id}
                        disabled={movingTaskID != null}
                        onSelect={() => selectCategory(category)}
                      />
                    ))}
                  </div>
                </ScrollArea>
              </div>
              <ScrollArea className="min-h-0">
                <FieldGroup className="p-6">
                  {selectedView !== "uncategorized" && (
                    <Field>
                      <FieldLabel htmlFor="task-category-name">분류 이름</FieldLabel>
                      <Input
                        id="task-category-name"
                        value={draftName}
                        onChange={(event) => setDraftName(event.target.value)}
                        placeholder="예: 외부 침투 테스트"
                        maxLength={80}
                        onKeyDown={(event) => {
                          if (event.key === "Enter") void saveCategory();
                        }}
                      />
                      <FieldDescription>
                        {selectedCategory
                          ? `이 분류를 쓰는 작업이 ${selectedCategory.task_count}개 있습니다. 이름을 바꾸면 작업 목록에도 반영됩니다.`
                          : "만든 뒤 새 작업 양식과 작업 목록 필터에서 쓸 수 있습니다."}
                      </FieldDescription>
                    </Field>
                  )}
                  {selectedView !== "new" && (
                    <Field>
                      <div className="flex flex-wrap items-end justify-between gap-2">
                        <div className="flex min-w-0 flex-col gap-1">
                          <FieldLabel>{selectedCategory ? "분류된 작업" : "미분류 작업"}</FieldLabel>
                          <FieldDescription>
                            {selectedCategory
                              ? `이 분류에 작업이 ${visibleTasks.length}개 있습니다.`
                              : `분류하지 않은 작업이 ${visibleTasks.length}개 있습니다.`}
                          </FieldDescription>
                        </div>
                      </div>
                      {visibleTasks.length === 0 ? (
                        <Empty className="min-h-36 border">
                          <EmptyHeader>
                            <EmptyTitle>
                              {selectedCategory ? "이 분류에 작업이 없습니다" : "미분류 작업이 없습니다"}
                            </EmptyTitle>
                            <EmptyDescription>작업을 이 분류에 넣으면 여기에 표시됩니다.</EmptyDescription>
                          </EmptyHeader>
                        </Empty>
                      ) : (
                        <ItemGroup className="gap-2">
                          {visibleTasks.map((task) => (
                            <DraggableCategoryTask
                              key={task.id}
                              task={task}
                              moving={movingTaskID === task.id}
                              disabled={movingTaskID != null}
                            />
                          ))}
                        </ItemGroup>
                      )}
                    </Field>
                  )}
                </FieldGroup>
              </ScrollArea>
            </div>
            <DragOverlay>{activeTask ? <CategoryTaskDragPreview task={activeTask} /> : null}</DragOverlay>
          </DndContext>
          <SheetFooter className="border-t px-6 py-4 sm:flex-row sm:items-center">
            {selectedCategory && (
              <Button
                type="button"
                variant="destructive"
                className="sm:mr-auto"
                disabled={saving || deleting}
                onClick={() => setDeleteOpen(true)}
              >
                <Trash2Icon data-icon="inline-start" />
                분류 삭제
              </Button>
            )}
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              닫기
            </Button>
            {selectedView !== "uncategorized" && (
              <Button
                type="button"
                disabled={!draftName.trim() || saving || deleting}
                onClick={() => void saveCategory()}
              >
                {saving ? <Spinner data-icon="inline-start" /> : <SaveIcon data-icon="inline-start" />}
                {saveLabel}
              </Button>
            )}
          </SheetFooter>
        </SheetContent>
      </Sheet>
      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>분류 "{selectedCategory?.name || "이름 없는 분류"}"을(를) 삭제할까요?</AlertDialogTitle>
            <AlertDialogDescription>
              분류를 삭제하면 그 안의 작업 {selectedCategory?.task_count ?? 0}개는 자동으로 "미분류"로 옮겨집니다. 작업
              데이터는 삭제되지 않습니다.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>취소</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={deleting}
              onClick={(event) => {
                event.preventDefault();
                void deleteCategory();
              }}
            >
              {deleting && <Spinner data-icon="inline-start" />}
              {deleting ? "삭제 중" : "삭제"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function CreateTaskSheet({
  tasks,
  categories,
  onCreated,
  onCategoriesChanged,
}: {
  tasks: Task[];
  categories: TaskCategory[];
  onCreated: () => void;
  onCategoriesChanged: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [name, setName] = React.useState("");
  const [categoryID, setCategoryID] = React.useState<number | undefined>(undefined);
  const [description, setDescription] = React.useState("");
  const [goal, setGoal] = React.useState("");
  const [selectedTemplateID, setSelectedTemplateID] = React.useState<number | null>(null);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [companies, setCompanies] = React.useState<Company[]>([]);
  const [sourceTaskIDs, setSourceTaskIDs] = React.useState<string[]>([]);
  const [companyIDs, setCompanyIDs] = React.useState<number[]>([]);
  const [llmProfileIDs, setLLMProfileIDs] = React.useState<string[]>([]);
  const [creating, setCreating] = React.useState(false);
  const [timeoutMin, setTimeoutMin] = React.useState(""); // 작업 단위 시간 초과(분). 비우거나 0이면 제한 없음
  const [heartbeatMin, setHeartbeatMin] = React.useState("10"); // 플래너 하트비트(분). 기본값 10, 최소 10(백엔드와 같다)
  const [seedFirstIntent, setSeedFirstIntent] = React.useState(false); // 생성할 때 첫 탐색 의도를 바로 내려 워커가 플래너의 첫 계획을 기다리지 않고 시작한다. 기본값 꺼짐이면 표준대로 계획한 뒤 실행한다
  const [coverageEnabled, setCoverageEnabled] = React.useState(true); // 자산 커버리지 기능. 기본값 켜짐. 끄면 커버리지를 계산·표시하지 않고, 범위를 누적하지 않고, 범위 관련 도구를 숨긴다(company 연결은 영향 없음)
  const [interceptRules, setInterceptRules] = React.useState<AssetInterceptRuleInput[]>([]); // 작업 단위 자산 차단 규칙(이 작업에만 적용되고 전역 표에 들어가지 않는다)
  // 방법 1 파일 업로드: 작업을 만들기 전에 파일을 drafts/<draftId>/uploads/에 임시 저장하고, 받은 절대 경로를 설명에 덧붙인다.
  const [uploading, setUploading] = React.useState(false);
  const [uploadCount, setUploadCount] = React.useState(0);
  const draftIdRef = React.useRef<string>("");
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const sheetContentRef = React.useRef<HTMLDivElement>(null);

  // load LLM profiles once for the create-task profile picker.
  React.useEffect(() => {
    api
      .llmProfiles()
      .then(setProfiles)
      .catch(() => setProfiles([]));
    api
      .companies()
      .then(setCompanies)
      .catch(() => setCompanies([]));
  }, []);

  // pickFiles uploads the chosen files into this draft's staging dir and appends their
  // absolute paths to the description; the task's agents open them by path via Read/Bash.
  async function pickFiles(files: FileList | null) {
    if (!files || files.length === 0) return;
    // crypto.randomUUID는 보안 컨텍스트(https/localhost)에서만 쓸 수 있어 IP+http로 접속하면 대체 방식을 쓴다.
    if (!draftIdRef.current) {
      draftIdRef.current =
        globalThis.crypto?.randomUUID?.() ?? `d${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;
    }
    setUploading(true);
    try {
      const r = await api.chatUpload("staging", draftIdRef.current, Array.from(files));
      setDescription((prev) => appendUploads(prev, r.attachments));
      setUploadCount((n) => n + r.attachments.length);
    } catch (e) {
      toast.error("파일을 업로드하지 못했습니다: " + (e as Error).message);
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = ""; // allow re-picking the same file
    }
  }

  async function createTask() {
    if (!description.trim() || !goal.trim()) {
      toast.error("설명과 목표를 입력하세요");
      return;
    }
    if (sourceTaskIDs.length > MAX_SOURCE_TASKS) {
      toast.error(`출처 작업은 최대 ${MAX_SOURCE_TASKS}개까지 연결할 수 있습니다`);
      return;
    }
    setCreating(true);
    try {
      const timeoutSec = Math.max(0, Math.floor(Number(timeoutMin) || 0)) * 60;
      const heartbeatSec = Math.max(10, Math.floor(Number(heartbeatMin) || 10)) * 60; // 최소 10분. 백엔드의 정규화와 같다
      await api.createTask({
        name: name.trim(),
        categoryId: categoryID,
        description: description.trim(),
        goal: goal.trim(),
        llmProfileIds: llmProfileIDs.map(Number),
        sourceTaskIds: sourceTaskIDs,
        companyIds: companyIDs,
        timeoutSeconds: timeoutSec,
        seedFirstIntent,
        planHeartbeatSeconds: heartbeatSec,
        coverageEnabled,
        interceptRules: interceptRules
          .map((r) => ({ ...r, pattern: r.pattern.trim() }))
          .filter((r) => r.pattern !== ""),
      });
      toast.success("작업을 만들었습니다");
      setName("");
      setCategoryID(undefined);
      setDescription("");
      setGoal("");
      setSelectedTemplateID(null);
      setSourceTaskIDs([]);
      setCompanyIDs([]);
      setLLMProfileIDs([]);
      setTimeoutMin("");
      setHeartbeatMin("10");
      setSeedFirstIntent(false);
      setCoverageEnabled(true);
      setInterceptRules([]);
      setUploadCount(0);
      draftIdRef.current = "";
      setOpen(false);
      onCreated();
    } catch (e) {
      toast.error("작업을 만들지 못했습니다: " + (e as Error).message);
    } finally {
      setCreating(false);
    }
  }

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button size="sm">
          <PlusIcon /> 새 작업
        </Button>
      </SheetTrigger>
      {/* 너비 45vw의 오른쪽 서랍. 화면 전체 높이로 스크롤되므로 긴 양식이 대화 상자 높이에 묶이지 않는다. 좁은 화면에서는 전체 너비가 된다.
            내용은 flex 열이다. 머리·바닥은 고정하고 가운데 필드 영역은 flex-1로 따로 스크롤한다. */}
      <SheetContent
        ref={sheetContentRef}
        side="right"
        className="w-full! max-w-none! gap-0 p-0 sm:w-[45vw]! sm:max-w-[45vw]!"
      >
        <SheetHeader className="border-b p-6">
          <SheetTitle>새 작업</SheetTitle>
          <SheetDescription>테스트 대상과 목표를 입력하세요. 고급 설정은 필요할 때 펼치세요.</SheetDescription>
        </SheetHeader>

        <div className="flex-1 overflow-y-auto p-6">
          <div className="grid gap-5">
            <TaskTemplateControls
              description={description}
              goal={goal}
              categoryID={categoryID}
              interceptRules={interceptRules}
              selectedTemplateID={selectedTemplateID}
              onSelectedTemplateIDChange={setSelectedTemplateID}
              onApply={(template) => {
                setDescription(template.description);
                setGoal(template.goal);
                setCategoryID(template.category_id ?? undefined);
                setInterceptRules(template.intercept_rules ?? []);
                setUploadCount(0);
              }}
              portalContainer={sheetContentRef}
            />
            <div className="grid gap-2">
              <Label htmlFor="name">이름(선택)</Label>
              <Input
                id="name"
                placeholder="예: Acme 공식 웹사이트 침투 테스트"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <Field>
              <FieldLabel htmlFor="task-category">작업 분류</FieldLabel>
              <CategoryPicker
                categories={categories}
                value={categoryID}
                onValueChange={setCategoryID}
                onCategoryCreated={onCategoriesChanged}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                선택. 분류는 하나만 고릅니다. 작업 목록 필터와 보관에 쓰며 에이전트 실행에는 영향이 없습니다.
              </FieldDescription>
            </Field>
            <div className="grid gap-2">
              <Label htmlFor="description">설명</Label>
              <Textarea
                id="description"
                className="min-h-32"
                placeholder="테스트 대상과 배경. 예: example.com 사이트 테스트"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
              />
              {/* 파일 업로드(여러 개 선택 가능): drafts/에 임시 저장하고 절대 경로를 위 설명에 덧붙인다. 워커는 그 경로를 Read/Bash로 연다. */}
              <div className="flex flex-wrap items-center gap-2">
                <input
                  ref={fileInputRef}
                  type="file"
                  multiple
                  className="hidden"
                  onChange={(e) => void pickFiles(e.target.files)}
                />
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => fileInputRef.current?.click()}
                  disabled={uploading}
                >
                  {uploading ? <Loader2Icon className="animate-spin" /> : <PaperclipIcon />}
                  파일 업로드
                </Button>
                <span className="text-muted-foreground text-xs">
                  {uploadCount > 0
                    ? `파일 ${uploadCount}개를 업로드했습니다. 절대 경로를 설명 끝에 덧붙였습니다(수정 가능).`
                    : "여러 개를 고를 수 있습니다. 업로드하면 파일의 절대 경로를 설명에 덧붙여 워커가 Read/Bash로 열 수 있게 합니다."}
                </span>
              </div>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="goal">목표</Label>
              <Textarea
                id="goal"
                className="min-h-32"
                placeholder="달성할 것. 예: 관리자 페이지 권한 얻기, 서버 권한 얻기"
                value={goal}
                onChange={(e) => setGoal(e.target.value)}
              />
            </div>
            <Field>
              <FieldLabel htmlFor="source-tasks">관련 작업</FieldLabel>
              <SourceTaskPicker
                tasks={tasks}
                value={sourceTaskIDs}
                onValueChange={setSourceTaskIDs}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                작업을 최대 {MAX_SOURCE_TASKS}개까지 연결합니다. 선택한 작업의 저장된 블랙보드, 자산 범위, 관련 트래픽을
                실시간 읽기 전용으로 이어받습니다. 새 작업은 자기 블랙보드에 씁니다.
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="task-companies">관련 기업 자산 범위</FieldLabel>
              <CompanyPicker
                companies={companies}
                value={companyIDs}
                onValueChange={setCompanyIDs}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                작업을 만들 때 선택한 기업의 현재 자산을 "테스트 자산"에 넣고, 도메인, IP, CIDR, ICP, 기업 키워드를
                에이전트에 범위 컨텍스트로 줍니다. 탐색 의도를 자동으로 만들거나 실행 목표를 강제로 바꾸지 않습니다.
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="task-intercept-rules">작업 단위 자산 차단 / 허용 규칙(선택)</FieldLabel>
              <AssetInterceptRulesEditor value={interceptRules} onChange={setInterceptRules} />
              <FieldDescription>
                이 작업에만 적용되고 전역 규칙에 쓰지 않습니다. 판정 순서: 먼저 "차단" 규칙(전역 포함)과 비교해 일치하면
                테스트를 금지합니다. 일치하지 않고 이 작업에 "허용" 규칙이 있으면 허용 규칙 중 하나와 일치해야 통과하고,
                그렇지 않으면 마찬가지로 테스트를 금지합니다. 허용 규칙이 하나도 없으면 허용 목록을 쓰지 않습니다.
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="llm-profiles">LLM 프로필 체인</FieldLabel>
              <TaskLLMProfileChain
                profiles={profiles}
                value={llmProfileIDs}
                onValueChange={setLLMProfileIDs}
                inputId="llm-profiles"
                portalContainer={sheetContentRef}
              />
              <FieldDescription>
                목록 순서대로 장애 조치합니다. 첫 항목이 현재 프로필이고, 사용 한도 부족이 확실할 때만 다음 항목으로
                바꿉니다.
              </FieldDescription>
            </Field>

            {/* 고급 설정(시간 초과, 하트비트, 첫 탐색 의도)은 기본으로 접어 두어 펼칠 때만 자리를 차지하게 하고 자주 쓰는 흐름을 간결하게 둔다. */}
            <Collapsible>
              <CollapsibleTrigger className="group flex w-full items-center gap-2 border-t pt-4 text-sm font-medium">
                <ChevronRightIcon className="text-muted-foreground size-4 transition-transform group-data-[state=open]:rotate-90" />
                고급 설정
                <span className="text-muted-foreground ml-auto text-xs font-normal">
                  시간 초과 · 하트비트 · 첫 탐색 의도
                </span>
              </CollapsibleTrigger>
              <CollapsibleContent className="grid gap-5 pt-5">
                <div className="grid gap-2">
                  <Label htmlFor="timeout-min">작업 시간 초과(분, 선택)</Label>
                  <Input
                    id="timeout-min"
                    type="number"
                    min={0}
                    className="w-40"
                    placeholder="비워 두면 제한 없음"
                    value={timeoutMin}
                    onChange={(e) => setTimeoutMin(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    시간이 되면 정상 마무리(각 에이전트의 결과 기록 + 플래너의 최종 판정)를 시작하고, 작업은 timeout
                    종료 상태가 됩니다.
                  </p>
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="heartbeat-min">플래너 하트비트(분)</Label>
                  <Input
                    id="heartbeat-min"
                    type="number"
                    min={10}
                    className="w-40"
                    placeholder="기본값 10"
                    value={heartbeatMin}
                    onChange={(e) => setHeartbeatMin(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    지난 계획이 끝나거나 작업이 시작된 뒤 이 시간 동안 아무 트리거가 없으면 계획을 한 번 자동으로
                    실행합니다(멈춤 대비 + 실행 중인 워커 점검). 최소 10분입니다.
                  </p>
                </div>
                <div className="grid gap-2">
                  <label htmlFor="seed-first-intent" className="flex items-center gap-2 text-sm">
                    <Checkbox
                      id="seed-first-intent"
                      checked={seedFirstIntent}
                      onCheckedChange={(v) => setSeedFirstIntent(!!v)}
                    />
                    첫 탐색 의도(설명+목표) 바로 내리기
                  </label>
                  <p className="text-muted-foreground text-xs">
                    켜면 작업을 만들 때 "설명+목표"를 탐색 의도 하나로 내려서, 워커가 첫 계획을 기다리지 않고 바로
                    시작하고 끝나면 플래너가 이어받아 판정·보완합니다. CTF처럼 워커 하나로 바로 풀리는 경우에
                    권장합니다. 끄면 표준대로 계획한 뒤 실행합니다.
                  </p>
                </div>
                <div className="grid gap-2">
                  <label htmlFor="coverage-enabled" className="flex items-center gap-2 text-sm">
                    <Checkbox
                      id="coverage-enabled"
                      checked={coverageEnabled}
                      onCheckedChange={(v) => setCoverageEnabled(!!v)}
                    />
                    자산 커버리지 기능
                  </label>
                  <p className="text-muted-foreground text-xs">
                    기본값 켜짐: 테스트 커버리지를 계산·표시하고, 현황 그래프에 테스트 진행률을 보이고, 테스트 범위를
                    자동으로 누적합니다. 끄면 커버리지를 계산·표시하지 않고, 현황 그래프는 진행률 없이 자산만 보이며,
                    에이전트도 범위 관련 도구를 받지 않습니다. 꺼도 "관련 기업 자산 범위"에는 영향이 없습니다.
                  </p>
                </div>
              </CollapsibleContent>
            </Collapsible>
          </div>
        </div>

        <SheetFooter className="flex-row justify-end gap-2 border-t p-4">
          <SheetClose asChild>
            <Button variant="outline">취소</Button>
          </SheetClose>
          <Button onClick={createTask} disabled={creating || uploading}>
            {creating && <Spinner data-icon="inline-start" />}
            {creating ? "만드는 중" : "만들기"}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}
