"use client";

import * as React from "react";

import { BellIcon, PlusIcon, SendIcon, Trash2Icon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { NotificationChannel, NotificationFilter, NotificationMeta } from "@/lib/types";

import {
  CHANNEL_FIELDS,
  type ChannelForm,
  emptyForm,
  KIND_LABEL,
  parseIDs,
  parseKeywords,
  parseKV,
  SEVERITY_OPTIONS,
} from "./_components/channel-fields";
import { ConfigField, FilterSummary } from "./_components/channel-form";
import { DeliveryList } from "./_components/delivery-list";
import { formatBacklog, StatTile } from "./_components/stat-tile";

// 이 페이지는 편성만 담당합니다: 데이터 로드, 폼 상태 유지, API 호출.
// 필드 정의와 파싱은 _components/channel-fields.ts, 컨트롤과 필터 요약이
// _components/channel-form.tsx, 전송 기록은 _components/delivery-list.tsx——
// 분리한 이유는 각각 따로 읽어도 이해되기 때문이며, 한 파일에 몰아넣으면 이 페이지가 1100 행.
export default function NotifyPage() {
  const [meta, setMeta] = React.useState<NotificationMeta | null>(null);
  const [channels, setChannels] = React.useState<NotificationChannel[]>([]);
  const [tab, setTab] = React.useState<"channels" | "deliveries">("channels");

  const [open, setOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<NotificationChannel | null>(null);
  const [form, setForm] = React.useState<ChannelForm>(emptyForm("dingtalk"));
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);

  const [globalSaving, setGlobalSaving] = React.useState(false);
  const [baseURL, setBaseURL] = React.useState("");
  const [digestMin, setDigestMin] = React.useState("");

  const load = React.useCallback(() => {
    api
      .notifyMeta()
      .then((m) => {
        setMeta(m);
        setBaseURL(m.public_base_url);
        setDigestMin(m.digest_interval_min);
      })
      .catch((e) => toast.error("푸시 설정 읽기 실패:" + (e as Error).message));
    // 채널 목록 로드 실패는 알려야 합니다: 무음 실패는 「채널이 하나도 없음」으로 표시되어,
    // 사용자가 설정이 사라졌다고 오해하게 되어, 직접 오류를 내는 것보다 더 당황스럽습니다.
    api
      .notifyChannels()
      .then(setChannels)
      .catch((e) => toast.error("채널 목록 읽기 실패:" + (e as Error).message));
  }, []);
  React.useEffect(() => {
    load();
  }, [load]);

  function setF(patch: Partial<ChannelForm>) {
    setForm((f) => ({ ...f, ...patch }));
  }
  function setCfg(key: string, value: unknown) {
    setForm((f) => ({ ...f, config: { ...f.config, [key]: value } }));
  }

  function openAdd() {
    setEditing(null);
    setForm(emptyForm(meta?.kinds[0]?.kind ?? "dingtalk"));
    setOpen(true);
  }

  function openEdit(ch: NotificationChannel) {
    setEditing(ch);
    // filter 백엔드에서는 Go 구조체로, 항상 객체로 직렬화됨(다음이 아님: null), 그래서 폴백이 필요 없습니다.
    const f = ch.filter;
    setForm({
      name: ch.name,
      kind: ch.kind,
      mode: ch.mode,
      enabled: ch.enabled,
      ratePerMin: String(ch.rate_per_min),
      // 백엔드가 되돌려준 config 의 자격 증명은 마스킹 값입니다; 폼에 그대로 넣고, 제출 시 그대로 되돌려 보내며,
      // 백엔드가 이를 바탕으로 DB의 원래 값을 유지합니다.
      config: { ...ch.config },
      minSeverity: f.min_severity ?? "",
      includeText: (f.vulnclass_include ?? []).join("\n"),
      excludeText: (f.vulnclass_exclude ?? []).join("\n"),
      taskIDsText: (f.task_ids ?? []).join(","),
      assetIDsText: (f.asset_ids ?? []).join(","),
      onStatusChange: f.on_status_change ?? false,
    });
    setOpen(true);
  }

  // buildConfig 폼 상태를 채널로 변환 config.
  //
  // 유일한 규칙, 두 가지 값:
  //   - 마스킹 값("__masked__...")를 그대로 되돌려 보냄 → 백엔드는 「이 필드는 수정 안 함, DB의 원래 값 유지」로 해석합니다
  //   - 나머지는 모두 사용자 입력대로 제출하며, 빈 문자열은 곧 「해당 필드 비우기」
  //
  // 자격 증명 필드를 특별히 취급하지 않는(예: 「자격 증명을 비우면 건너뜀」) 이유는, 그러면 사용자가**지울 수 없음**
  // 잘못 설정한 키 하나——화면에 「이것을 삭제하겠다」를 표현할 수 있는 동작이 없습니다. 현재 규칙에서는,
  // 입력란을 비우면 해당 필드를 비우는 것과 같아, 의미가 유일하고 사용자가 제어 가능합니다.
  // 마스킹 값은 입력란에 나타나지 않음(참조: ConfigField), 그래서 「칸에 글자가 있음」은 항상 다음과 같음:
  // 「사용자가 직접 입력한 것」.
  function buildConfig(): Record<string, unknown> {
    const defs = CHANNEL_FIELDS[form.kind] ?? [];
    const out: Record<string, unknown> = {};
    for (const d of defs) {
      const raw = form.config[d.key];
      if (d.kind === "switch") {
        out[d.key] = raw === true;
        continue;
      }
      if (typeof raw === "string" && raw.startsWith("__masked__")) {
        out[d.key] = raw;
        continue;
      }
      if (d.kind === "number") {
        const n = Number(raw);
        out[d.key] = Number.isFinite(n) && n > 0 ? n : 0;
        continue;
      }
      if (d.kind === "kv") {
        out[d.key] = parseKV(String(raw ?? ""));
        continue;
      }
      if (d.kind === "list") {
        out[d.key] = String(raw ?? "")
          .split(/[\s,,]+/)
          .map((s) => s.trim())
          .filter(Boolean);
        continue;
      }
      out[d.key] = String(raw ?? "").trim();
    }
    return out;
  }

  function buildFilter(): NotificationFilter {
    return {
      min_severity: form.minSeverity || undefined,
      vulnclass_include: parseKeywords(form.includeText),
      vulnclass_exclude: parseKeywords(form.excludeText),
      task_ids: parseIDs(form.taskIDsText),
      asset_ids: parseIDs(form.assetIDsText),
      on_status_change: form.onStatusChange,
    };
  }

  async function saveForm() {
    if (!form.name.trim()) {
      toast.error("채널 이름을 입력하세요");
      return;
    }
    setSaving(true);
    try {
      const payload = {
        name: form.name.trim(),
        kind: form.kind,
        mode: form.mode,
        enabled: form.enabled,
        config: buildConfig(),
        filter: buildFilter(),
        rate_per_min: form.ratePerMin.trim() === "" ? undefined : Number(form.ratePerMin),
      };
      if (editing) {
        await api.notifyUpdateChannel(editing.id, payload);
        toast.success("저장됨");
        setOpen(false);
      } else {
        await api.notifyCreateChannel(payload);
        toast.success("채널을 추가했습니다");
        setOpen(false);
      }
      load();
    } catch (e) {
      toast.error("저장 실패:" + (e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function testChannel() {
    if (!editing) return;
    setTesting(true);
    try {
      const r = await api.notifyTestChannel(editing.id);
      toast.success(`테스트 메시지를 발송했습니다(${r.latency_ms} ms), 그룹에서 확인하세요`);
    } catch (e) {
      // 백엔드가 채널이 반환한 원본 오류를 그대로 전달하며, 이것이 설정을 진단하는 유일한 단서이므로 원본 그대로 표시합니다.
      toast.error("테스트 실패:" + (e as Error).message, { duration: 12000 });
    } finally {
      setTesting(false);
    }
  }

  async function removeChannel(ch: NotificationChannel) {
    try {
      await api.notifyDeleteChannel(ch.id);
      toast.success(`삭제됨:${ch.name}`);
      setOpen(false);
      load();
    } catch (e) {
      toast.error("삭제 실패:" + (e as Error).message);
    }
  }

  async function toggleEnabled(ch: NotificationChannel) {
    try {
      await api.notifyUpdateChannel(ch.id, { enabled: !ch.enabled });
      load();
    } catch (e) {
      toast.error("작업 실패:" + (e as Error).message);
    }
  }

  async function toggleGlobal(on: boolean) {
    setGlobalSaving(true);
    try {
      await api.setSettings({ notify_enabled: on });
      setMeta((m) => (m ? { ...m, enabled: on } : m));
      toast.success(on ? "푸시가 켜졌습니다" : "푸시가 일시정지되었습니다");
    } catch (e) {
      toast.error("작업 실패:" + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  async function saveGlobal() {
    setGlobalSaving(true);
    try {
      const patch: Record<string, unknown> = { notify_public_base_url: baseURL.trim() };
      const n = Number(digestMin);
      if (Number.isFinite(n) && n > 0) patch.notify_digest_interval_min = n;
      await api.setSettings(patch);
      toast.success("저장됨");
      load();
    } catch (e) {
      toast.error("저장 실패:" + (e as Error).message);
    } finally {
      setGlobalSaving(false);
    }
  }

  const fields = CHANNEL_FIELDS[form.kind] ?? [];
  const secretKeys = new Set(meta?.kinds.find((k) => k.kind === form.kind)?.secret_keys ?? []);
  const defaultRate = meta?.kinds.find((k) => k.kind === form.kind)?.default_rate_per_min ?? 0;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">알림 푸시</h1>
          <p className="text-muted-foreground text-sm">
            취약점 발견 시 딩톡으로 푸시 / 페이슈 / 기업 위챗 등 채널 · 각 채널은 푸시 시점과 필터 규칙을 독립적으로 설정 가능
          </p>
        </div>
        {meta && (
          // 사용 div 가 아니라 label:Switch 자체 포함 aria-label, 바깥에 한 겹 더 label
          // 어떤 네이티브 컨트롤에도 연결되지 않으면서, 텍스트 클릭이 전환 가능한 것처럼 보이게 합니다.
          <div className="flex shrink-0 items-center gap-2 text-sm">
            <span className="text-muted-foreground">메인 스위치</span>
            <Switch
              checked={meta.enabled}
              disabled={globalSaving}
              onCheckedChange={toggleGlobal}
              aria-label="푸시 메인 스위치"
            />
          </div>
        )}
      </div>

      {meta && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <StatTile label="채널" value={`${meta.stats.channels_on} / ${meta.stats.channels}`} hint="사용 / 총수" />
          <StatTile label="오늘 전송됨" value={String(meta.stats.sent_today)} />
          <StatTile label="전송 대기" value={String(meta.stats.pending)} />
          <StatTile label="실패" value={String(meta.stats.failed)} tone={meta.stats.failed > 0 ? "red" : undefined} />
          <StatTile
            label="최장 백로그"
            value={formatBacklog(meta.stats.backlog_age_ms)}
            // 백로그 경과 시간이 백로그 건수보다 훨씬 유용함: 백로그 3 건은 다음에서 올 수 있음: 3 초 내 도달 3 시간.
            hint={meta.stats.backlog_age_ms > 5 * 60_000 ? "푸시가 막혔을 수 있음" : undefined}
            tone={meta.stats.backlog_age_ms > 5 * 60_000 ? "red" : undefined}
          />
        </div>
      )}

      <Card className="gap-3">
        <CardHeader>
          <CardTitle className="text-base">전역 설정</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          <div className="grid gap-2">
            <Label htmlFor="n-base">링크 주소</Label>
            <Input
              id="n-base"
              placeholder="https://artex.example.com"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">메시지의 「상세 보기」 버튼이 가리키는 주소. 비우면 버튼 없음.</p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="n-digest">요약 주기(분)</Label>
            <Input
              id="n-digest"
              type="number"
              min={1}
              max={1440}
              placeholder="30"
              value={digestMin}
              onChange={(e) => setDigestMin(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">「요약」 모드 채널에만 적용됩니다.</p>
          </div>
          <div className="sm:col-span-2">
            <Button onClick={saveGlobal} disabled={globalSaving}>
              전역 설정 저장
            </Button>
          </div>
        </CardContent>
      </Card>

      <Tabs value={tab} onValueChange={(v) => setTab(v as "channels" | "deliveries")} className="flex flex-col gap-4">
        <TabsList>
          <TabsTrigger value="channels">채널</TabsTrigger>
          <TabsTrigger value="deliveries">전송 기록</TabsTrigger>
        </TabsList>

        <TabsContent value="channels">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <button
              type="button"
              onClick={openAdd}
              className="text-foreground/70 border-foreground/70 hover:bg-muted/60 hover:shadow-sm flex min-h-[130px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed transition"
            >
              <PlusIcon className="size-6" />
              <span className="text-sm">채널 추가</span>
            </button>

            {channels.map((ch) => (
              <Card
                key={ch.id}
                onClick={() => openEdit(ch)}
                className="hover:border-primary/60 cursor-pointer gap-3 transition hover:shadow-sm"
              >
                <CardHeader>
                  <div className="flex items-center gap-2">
                    <BellIcon className="text-muted-foreground size-4 shrink-0" />
                    <CardTitle className="truncate text-base">{ch.name}</CardTitle>
                    {/* 카드 전체가 클릭 가능(편집 진입)하므로, 이 두 컨트롤은 각각 버블링을 삼켜야 하며,
                        그러지 않으면 스위치/삭제가 함께 편집을 트리거합니다. stopPropagation 컨트롤 자체에 부착
                        에, 한 겹 씌우는 대신 div: 씌움 div 「상호작용 가능해 보이지만 없는
                        역할」 정적 요소로, 트리거도 하고 a11y 경고로, 의미상으로도 맞지 않습니다. */}
                    <div className="ml-auto flex items-center gap-2">
                      <Switch
                        checked={ch.enabled}
                        onCheckedChange={() => toggleEnabled(ch)}
                        onClick={(e) => e.stopPropagation()}
                        aria-label="사용"
                      />
                      <Button
                        size="icon"
                        variant="outline"
                        aria-label="삭제"
                        onClick={(e) => {
                          e.stopPropagation();
                          // void 명시적 폐기 Promise:removeChannel 자체 catch 그리고 toast,
                          // 여기서는 필요 없음 await(onClick 이(가) 아님 async).
                          void removeChannel(ch);
                        }}
                      >
                        <Trash2Icon className="text-destructive" />
                      </Button>
                    </div>
                  </div>
                </CardHeader>
                <CardContent className="grid gap-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline">{KIND_LABEL[ch.kind] ?? ch.kind}</Badge>
                    <Badge variant="outline">{ch.mode === "digest" ? "요약" : "실시간"}</Badge>
                    {!ch.enabled && <Badge variant="outline">비활성화됨</Badge>}
                  </div>
                  <FilterSummary filter={ch.filter} />
                </CardContent>
              </Card>
            ))}
          </div>
        </TabsContent>

        <TabsContent value="deliveries">
          <DeliveryList channels={channels} />
        </TabsContent>
      </Tabs>

      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="right" className="w-full data-[side=right]:sm:max-w-lg">
          <SheetHeader>
            <SheetTitle>{editing ? editing.name : "알림 채널 추가"}</SheetTitle>
            <SheetDescription>
              {KIND_LABEL[form.kind] ?? form.kind}
              {defaultRate > 0 ? ` · 기본 속도 제한 ${defaultRate} 건/분` : " · 속도 제한 없음"}
            </SheetDescription>
          </SheetHeader>

          <div className="flex min-h-0 flex-1 flex-col overflow-y-auto px-4">
            <div className="grid gap-4 py-4">
              <div className="grid gap-2">
                <Label>채널 유형</Label>
                <Select
                  value={form.kind}
                  onValueChange={(v) => {
                    // 유형을 바꾸면 자격 증명 필드 세트를 바꾸는 것과 같아, 이전 설정을 병합할 수 없습니다.
                    setF({ kind: v, config: {} });
                  }}
                  disabled={!!editing}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {(meta?.kinds ?? []).map((k) => (
                      <SelectItem key={k.kind} value={k.kind}>
                        {KIND_LABEL[k.kind] ?? k.kind}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {editing && (
                  <p className="text-muted-foreground text-xs">
                    채널 유형은 수정할 수 없음——유형을 바꾸면 자격 증명 세트를 바꾸는 것과 같으니, 채널을 새로 만드세요.
                  </p>
                )}
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-name">채널 이름</Label>
                <Input
                  id="n-name"
                  placeholder="긴급 대응 그룹 / 일상 공지 그룹"
                  value={form.name}
                  onChange={(e) => setF({ name: e.target.value })}
                />
              </div>

              {fields.length === 0 ? (
                <p className="text-muted-foreground text-sm">
                  이 채널의 폼이 아직 정의되지 않음(프런트엔드에 없음: CHANNEL_FIELDS 항목), 보완한 뒤 다시 시도하세요.
                </p>
              ) : (
                fields.map((d) => (
                  <ConfigField
                    key={d.key}
                    def={d}
                    value={form.config[d.key]}
                    isSecret={secretKeys.has(d.key)}
                    onChange={(v) => setCfg(d.key, v)}
                  />
                ))
              )}

              <div className="grid gap-2">
                <Label>푸시 시점</Label>
                <Select value={form.mode} onValueChange={(v) => setF({ mode: v as "realtime" | "digest" })}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="realtime">실시간 · 취약점마다 개별 발송</SelectItem>
                    <SelectItem value="digest">요약 · 주기별로 하나로 병합</SelectItem>
                  </SelectContent>
                </Select>
                <p className="text-muted-foreground text-xs">
                  「높음은 실시간, 나머지는 요약」을 하려면 채널 두 개를 만드세요: 하나는 실시간 + 임계값 높음, 하나는 요약 + 레벨 제한 없음.
                </p>
              </div>

              <div className="grid gap-2">
                <Label htmlFor="n-rate">속도 제한(건/분)</Label>
                <Input
                  id="n-rate"
                  type="number"
                  min={0}
                  placeholder={defaultRate > 0 ? String(defaultRate) : "0 = 무제한"}
                  value={form.ratePerMin}
                  onChange={(e) => setF({ ratePerMin: e.target.value })}
                />
                <p className="text-muted-foreground text-xs">
                  비우면 채널 기본값 사용;0 속도 제한 없음을 의미. 초과해도 메시지를 버리지 않고 전송을 지연할 뿐입니다.
                </p>
              </div>

              <div className="border-t pt-4">
                <p className="mb-3 text-sm font-medium">필터 규칙(비우면 필터링 안 함)</p>
                <div className="grid gap-4">
                  <div className="grid gap-2">
                    <Label>최저 레벨</Label>
                    <Select
                      value={form.minSeverity || "all"}
                      onValueChange={(v) => setF({ minSeverity: v === "all" ? "" : v })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {SEVERITY_OPTIONS.map((o) => (
                          <SelectItem key={o.value || "all"} value={o.value || "all"}>
                            {o.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-inc">이 취약점 유형만 푸시</Label>
                    <Textarea
                      id="n-inc"
                      placeholder={"SQL주입\n명령 실행"}
                      value={form.includeText}
                      onChange={(e) => setF({ includeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">
                      한 줄에 키워드 하나, 대소문자 구분 없는 부분 문자열 매칭. 비우면=전체 유형.
                    </p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-exc">이 취약점 유형 제외</Label>
                    <Textarea
                      id="n-exc"
                      placeholder={"정보 유출"}
                      value={form.excludeText}
                      onChange={(e) => setF({ excludeText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">제외가 포함보다 우선: 동시에 적중하면 제외됩니다.</p>
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-tasks">작업 한정 ID</Label>
                    <Input
                      id="n-tasks"
                      placeholder="1, 2, 3"
                      value={form.taskIDsText}
                      onChange={(e) => setF({ taskIDsText: e.target.value })}
                    />
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="n-assets">자산 한정 ID</Label>
                    <Input
                      id="n-assets"
                      placeholder="10, 11"
                      value={form.assetIDsText}
                      onChange={(e) => setF({ assetIDsText: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">작업/자산 비우면=제한 없음; 입력하면 취약점과 교집합이 있어야 함.</p>
                  </div>
                  <div className="flex items-center gap-2 text-sm">
                    <Switch
                      checked={form.onStatusChange}
                      onCheckedChange={(v) => setF({ onStatusChange: v })}
                      aria-label="상태 변경 수신"
                    />
                    취약점 처리 상태 변경 시에도 푸시(실시간 모드만)
                  </div>
                </div>
              </div>

              <div className="flex items-center gap-2 text-sm">
                <Switch checked={form.enabled} onCheckedChange={(v) => setF({ enabled: v })} aria-label="사용" />
                이 채널 사용
              </div>
            </div>

            <div className="flex gap-2 pt-2 pb-6">
              <Button onClick={saveForm} disabled={saving}>
                {editing ? "저장" : "추가"}
              </Button>
              {editing && (
                <Button variant="outline" onClick={testChannel} disabled={testing}>
                  <SendIcon /> 테스트 메시지 전송
                </Button>
              )}
            </div>
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
