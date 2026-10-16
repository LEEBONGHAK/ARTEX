"use client";

import * as React from "react";

import { CpuIcon, FlaskConicalIcon, KeyboardIcon, RadioTowerIcon, SearchIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import { CHAT_SEND_MODE_OPTIONS, type ChatSendMode, setChatSendMode, useChatSendMode } from "@/lib/chat-send-mode";
import type { Settings } from "@/lib/types";

import { UpdateCard } from "./_components/update-card";

export default function SystemSettingsPage() {
  const [trafficCapture, setTrafficCapture] = React.useState(false);
  const [agentTrafficBinding, setAgentTrafficBinding] = React.useState(false);
  const [webSearch, setWebSearch] = React.useState(false);
  const [backend, setBackend] = React.useState("ddgs");
  const [braveKeySet, setBraveKeySet] = React.useState(false);
  const [braveKeyInput, setBraveKeyInput] = React.useState("");
  const [tavilyKeySet, setTavilyKeySet] = React.useState(false);
  const [tavilyKeyInput, setTavilyKeyInput] = React.useState("");
  const [savingTavilyKey, setSavingTavilyKey] = React.useState(false);
  const [proxyInput, setProxyInput] = React.useState("");
  const [savingProxy, setSavingProxy] = React.useState(false);
  const [globalProxyInput, setGlobalProxyInput] = React.useState("");
  const [savingGlobalProxy, setSavingGlobalProxy] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [loaded, setLoaded] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [savingKey, setSavingKey] = React.useState(false);
  const [pyInterp, setPyInterp] = React.useState("");
  const [workers, setWorkers] = React.useState("3");
  const [savingWorkers, setSavingWorkers] = React.useState(false);
  // 작업 제약 주입 범위(기본적으로 모두 켜짐).
  const [injectPlanner, setInjectPlanner] = React.useState(true);
  const [injectWorker, setInjectWorker] = React.useState(true);
  // 실험 기능:noa 컨텍스트 압축(기본 꺼짐).
  const [noaCompaction, setNoaCompaction] = React.useState(false);
  // 순수 프런트엔드 환경 설정: 거치지 않음 /api/settings, 직접 읽기·쓰기 localStorage.
  const sendMode = useChatSendMode();

  const apply = React.useCallback((s: Settings) => {
    setTrafficCapture(!!s.traffic_capture);
    setAgentTrafficBinding(!!s.agent_traffic_binding);
    setWebSearch(!!s.web_search_enabled);
    setBackend(s.web_search_backend || "ddgs");
    setBraveKeySet(!!s.brave_key_set);
    setTavilyKeySet(!!s.tavily_key_set);
    setProxyInput(s.web_search_proxy ?? "");
    setGlobalProxyInput(s.global_proxy ?? "");
    setPyInterp(s.python_interpreter ?? "");
    setWorkers(String(s.workers ?? 3));
    setInjectPlanner(s.constraints_inject_planner !== false);
    setInjectWorker(s.constraints_inject_worker !== false);
    setNoaCompaction(!!s.noa_compaction);
  }, []);

  const saveWorkers = () => {
    const n = Number(workers);
    if (!Number.isInteger(n) || n <= 0) {
      toast.error("동시 실행 수는 다음보다 커야 함: 0 인 정수");
      return;
    }
    setSavingWorkers(true);
    api
      .setSettings({ workers: n })
      .then((s) => {
        apply(s);
        toast.success("동시 실행 Worker 수를 저장했습니다 agent 수(이후 시작되는 작업에 적용)");
      })
      .catch((e) => toast.error("저장 실패:" + (e as Error).message))
      .finally(() => setSavingWorkers(false));
  };

  const savePython = () => {
    setSaving(true);
    api
      .setSettings({ python_interpreter: pyInterp.trim() })
      .then((s) => {
        apply(s);
        toast.success("저장됨 Python 인터프리터 설정");
      })
      .catch((e) => toast.error("저장 실패:" + (e as Error).message))
      .finally(() => setSaving(false));
  };
  const detectPython = () => {
    setSaving(true);
    api
      .detectPython()
      .then((r) => setPyInterp(r.python_interpreter))
      .catch(() => undefined)
      .finally(() => setSaving(false));
  };

  React.useEffect(() => {
    api
      .settings()
      .then(apply)
      .catch(() => undefined)
      .finally(() => setLoaded(true));
  }, [apply]);

  const toggleTraffic = (v: boolean) => {
    setTrafficCapture(v); // optimistic
    setSaving(true);
    api
      .setSettings({ traffic_capture: v })
      .then(apply)
      .catch(() => setTrafficCapture(!v)) // revert on failure
      .finally(() => setSaving(false));
  };

  const toggleInjectPlanner = (v: boolean) => {
    setInjectPlanner(v); // optimistic
    api
      .setSettings({ constraints_inject_planner: v })
      .then(apply)
      .catch(() => setInjectPlanner(!v)); // revert on failure
  };

  const toggleAgentTrafficBinding = (v: boolean) => {
    setAgentTrafficBinding(v);
    setSaving(true);
    api
      .setSettings({ agent_traffic_binding: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "켜짐 Agent 트래픽 자동 바인딩" : "꺼짐 Agent 트래픽 자동 바인딩");
      })
      .catch((e) => {
        setAgentTrafficBinding(!v);
        toast.error(`저장 실패:${(e as Error).message}`);
      })
      .finally(() => setSaving(false));
  };

  const toggleInjectWorker = (v: boolean) => {
    setInjectWorker(v); // optimistic
    api
      .setSettings({ constraints_inject_worker: v })
      .then(apply)
      .catch(() => setInjectWorker(!v)); // revert on failure
  };

  const toggleNoaCompaction = (v: boolean) => {
    setNoaCompaction(v); // optimistic
    api
      .setSettings({ noa_compaction: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "켜짐 noa 컨텍스트 압축(이후 시작되는 실행에 적용)" : "꺼짐 noa 컨텍스트 압축(내장 압축으로 복원)");
      })
      .catch((e) => {
        setNoaCompaction(!v); // revert on failure
        toast.error(`저장 실패:${(e as Error).message}`);
      });
  };

  // Persist a web-search patch (enable and/or backend). Optimistic with refetch.
  const saveWebSearch = (patch: Partial<Settings>) => {
    setSaving(true);
    api
      .setSettings(patch)
      .then((s) => {
        apply(s);
        toast.success("웹 검색 설정을 저장했습니다");
      })
      .catch((e) => {
        toast.error("저장 실패:" + (e as Error).message);
        api
          .settings()
          .then(apply)
          .catch(() => undefined);
      })
      .finally(() => setSaving(false));
  };

  const saveBraveKey = () => {
    setSavingKey(true);
    api
      .setSettings({ brave_search_api_key: braveKeyInput })
      .then((s) => {
        apply(s);
        setBraveKeyInput("");
        toast.success("저장됨 Brave API Key");
      })
      .catch((e) => toast.error("저장 실패:" + (e as Error).message))
      .finally(() => setSavingKey(false));
  };

  const saveTavilyKey = () => {
    setSavingTavilyKey(true);
    api
      .setSettings({ tavily_search_api_key: tavilyKeyInput })
      .then((s) => {
        apply(s);
        setTavilyKeyInput("");
        toast.success("저장됨 Tavily API Key");
      })
      .catch((e) => toast.error("저장 실패:" + (e as Error).message))
      .finally(() => setSavingTavilyKey(false));
  };

  const saveProxy = () => {
    setSavingProxy(true);
    api
      .setSettings({ web_search_proxy: proxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(proxyInput.trim() ? "출구 프록시를 저장했습니다" : "출구 프록시를 해제했습니다(직접 연결로 변경)");
      })
      .catch((e) => toast.error("저장 실패:" + (e as Error).message))
      .finally(() => setSavingProxy(false));
  };

  const saveGlobalProxy = () => {
    setSavingGlobalProxy(true);
    api
      .setSettings({ global_proxy: globalProxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(globalProxyInput.trim() ? "전역 프록시를 저장했습니다" : "전역 프록시를 해제했습니다(직접 연결로 변경)");
      })
      .catch((e) => toast.error("저장 실패:" + (e as Error).message))
      .finally(() => setSavingGlobalProxy(false));
  };

  // Run a real "test" search ("test") against the CURRENT form values (backend +
  // proxy + entered key), falling back to saved values server-side. Toasts result.
  const runTest = () => {
    setTesting(true);
    api
      .testWebSearch({
        web_search_backend: backend,
        web_search_proxy: proxyInput.trim(),
        brave_search_api_key: braveKeyInput,
        tavily_search_api_key: tavilyKeyInput,
      })
      .then((r) => {
        if (r.ok) toast.success(`검색 테스트 성공 · ${r.backend} 반환 ${r.count} 건 결과`);
        else toast.error("검색 테스트 실패:" + (r.error || "알 수 없는 오류"));
      })
      .catch((e) => toast.error("검색 테스트 실패:" + (e as Error).message))
      .finally(() => setTesting(false));
  };

  // brave-free selected but no key stored and none being entered → tool stays off.
  const braveNeedsKey = webSearch && backend === "brave-free" && !braveKeySet;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">시스템 설정</h1>
        <p className="text-muted-foreground text-sm">전역 런타임 스위치</p>
      </div>

      {/* 다중 열, 단일이 아니라 grid: 웹 검색 카드는 나머지보다 몇 배 높고, 높이는 선택한 백엔드에 따라 달라집니다(brave/tavily
          의 key 입력은 조건부 렌더링).grid 가장 높은 카드에 맞춰 행 전체를 채워 옆에 큰 여백이 생기므로,
          여러 열이면 내용 높이에 맞춰 자동으로 균형 있게 채웁니다. 카드 간격은 mb 가 아니라 gap——다중 열 레이아웃에서
          column-gap : 매 검색마다 모델 호출을 한 번 추가로 소모합니다(발생 */}
      <div className="columns-1 gap-4 md:gap-6 lg:columns-2">
        <UpdateCard />

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              트래픽 캡처
            </CardTitle>
            <CardDescription>
              켜면 모든 Agent 의 HTTP 트래픽이 기록 프록시를 거쳐 전량 DB에 저장되고, Agent 주입 traffic_search / traffic_get
              도구와 프록시 설정(프롬프트에 프록시 설명 포함).
              <br />
              꺼짐(기본)일 때는 어떤 트래픽도 기록하지 않습니다:Agent
              <b>하지 않음</b>프록시 설정과 트래픽 도구를 받고, 프롬프트도<b>미포함</b>프록시 관련 내용. 전환하면 즉시 재구축됩니다 Agent 적용됩니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="traffic-capture" className="text-sm font-normal text-muted-foreground">
              {trafficCapture ? "켜짐 · 트래픽을 기록하고 프록시를 주입하는 중" : "꺼짐 · 기록 안 함, 프록시 주입 안 함"}
            </Label>
            <Switch
              id="traffic-capture"
              checked={trafficCapture}
              disabled={!loaded || saving}
              onCheckedChange={toggleTraffic}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              Agent 트래픽 자동 바인딩
            </CardTitle>
            <CardDescription id="agent-traffic-binding-description">
              열 간격만 담당하며, 행 간격은 자식 요소가 직접 지정해야 합니다. Agent 기존 값을 대조함 HTTP 요청/응답하고, 해당 트래픽을 연결한 뒤 리포트를 작성합니다.
              <b>패킷 조회 및 추가 도구 호출이 늘립니다: Token 소모합니다.</b>
              <br />
              TCP, 패킷을 캡처하지 않았거나 일치하는 트래픽이 없어도 정상적으로 보고할 수 있습니다. 이 스위치는 트래픽 캡처, 수동 바인딩 및 저장된 증거 조회에 영향을 주지 않습니다. 다음 라운드에 Agent
              적용됩니다; 끄면 새 자동 바인딩을 즉시 거부합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="agent-traffic-binding" className="text-sm font-normal text-muted-foreground">
              {agentTrafficBinding ? "켜짐 · 늘어납니다 Token 소모" : "꺼짐 · 수동 바인딩을 계속할 수 있습니다"}
            </Label>
            <Switch
              id="agent-traffic-binding"
              aria-describedby="agent-traffic-binding-description"
              checked={agentTrafficBinding}
              disabled={!loaded || saving}
              onCheckedChange={toggleAgentTrafficBinding}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              전역 프록시
            </CardTitle>
            <CardDescription>
              모든 Agent 의<b>대상 트래픽</b>이 프록시를 거쳐 외부로 나갑니다(소스 숨김 IP / 점프 서버 경유). 지원: <b>http / https / socks5</b>, 포함 가능:{" "}
              <code>user:pass</code> 인증. 비워 두면=직접 연결.
              <br />
              켜기<b>트래픽 캡처</b>일 때, 기록 프록시의<b>업스트림</b>(트래픽은 여전히 전량 DB에 저장되고, 이 프록시를 거쳐 외부로 나갑니다); 캡처를 끄면 직접 주입
              Agent 의 bash / WebFetch 외부로 나갑니다. 웹 검색 프록시,LLM 프록시는 서로 독립적입니다.
              <br />
              <b>프롬프트</b>:socks5 에서<b>캡처 끄기</b>일 때 각 명령줄 도구의 <code>ALL_PROXY</code> 지원(curl
              사용 가능하나 일부 도구는 무시할 수 있음); 주로 사용한다면 socks5, 트래픽 캡처를 켜기를 권장합니다——이 경로는 MITM
              직접 다이얼하여, 도구는 인지하지 못하고 안정적으로 적용됩니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            <Label htmlFor="global-proxy" className="text-sm font-normal text-muted-foreground">
              프록시 주소
            </Label>
            <div className="flex items-center gap-2">
              <Input
                id="global-proxy"
                autoComplete="off"
                placeholder="socks5://user:pass@host:1080 또는 http://host:port(비워 두면=직접 연결)"
                value={globalProxyInput}
                disabled={!loaded || savingGlobalProxy}
                onChange={(e) => setGlobalProxyInput(e.target.value)}
              />
              <Button type="button" onClick={saveGlobalProxy} disabled={!loaded || savingGlobalProxy}>
                저장
              </Button>
            </div>
            <p className="text-muted-foreground text-xs">
              {globalProxyInput.trim() ? "설정됨 · 모든 대상 트래픽이 이 프록시를 거쳐 외부로 나갑니다" : "미설정 · 대상 트래픽이 직접 연결로 외부로 나갑니다"}
            </p>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <ShieldAlertIcon className="size-4" />
              작업 제약 주입
            </CardTitle>
            <CardDescription>
              켜면 각 작업의<b>작업 제약</b>(작업 총괄의 「작업 제약」에서 관리하는 allow/deny 항목)을 해당 위치에 결합 Agent
              의 시스템 프롬프트로, 탐색 경계를 한정하는 데 사용합니다(예: 「현재 포트만 테스트」 「브루트포스 금지」).
              <br />
              다음에 각각 주입 제어 가능: <b>Planner(planner)</b>와 <b>Worker(worker)</b>
              ; 기본적으로 모두 켜짐. 전환은 즉시 적용되며(다음 라운드에서 읽음), 재구축이 필요 없습니다 Agent. 끄면 해당 Agent 더 이상 제약을 보지 못합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-planner" className="text-sm font-normal text-muted-foreground">
                Planner에 주입(planner){injectPlanner ? " · 켜짐" : " · 꺼짐"}
              </Label>
              <Switch
                id="inject-planner"
                checked={injectPlanner}
                disabled={!loaded}
                onCheckedChange={toggleInjectPlanner}
              />
            </div>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-worker" className="text-sm font-normal text-muted-foreground">
                Worker에 주입(worker){injectWorker ? " · 켜짐" : " · 꺼짐"}
              </Label>
              <Switch
                id="inject-worker"
                checked={injectWorker}
                disabled={!loaded}
                onCheckedChange={toggleInjectWorker}
              />
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <FlaskConicalIcon className="size-4" />
              실험 기능
            </CardTitle>
            <CardDescription>
              아직 검증 중인 메커니즘으로 기본 꺼짐. 다음을 바꿀 수 있음: Agent 기본 꺼짐. 켜면 취약점 저장 시 트리거되는 리포트
              <br />
              <b>noa 컨텍스트 압축</b>: 모델이 긴 대화 기록을 능동적으로 압축(norma v0.4.0). 켜면 플랫폼에 연결된 네 종류의 Agent(
              <b>Planner / Worker / 메인 Agent / 대화</b>)는 다음으로 변경: noa 컨텍스트를 인계받아 내장 압축을 대체하며,
              압축 원문은 작업 작업 디렉터리에 아카이브되어 추적이 쉽습니다. 전환은 즉시 적용되며(이후 시작되는 실행에 적용), 재구축이 필요 없습니다 Agent;
              끄면 즉시 내장 압축으로 복원됩니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="noa-compaction" className="text-sm font-normal text-muted-foreground">
              noa 컨텍스트 압축{noaCompaction ? " · 켜짐" : " · 꺼짐"}
            </Label>
            <Switch
              id="noa-compaction"
              checked={noaCompaction}
              disabled={!loaded}
              onCheckedChange={toggleNoaCompaction}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <SearchIcon className="size-4" />
              웹 검색
            </CardTitle>
            <CardDescription>
              이것은 웹 검색의<b>메인 스위치 + 소스 설정</b>. 켜야만 다음에서 가능:<b>각 Agent 설정</b>에서 개별적으로 사용 여부 선택
              <b>web_search</b>(제목만 반환/링크/요약이며 본문은 가져오지 않습니다; 가져오기는 WebFetch 담당). 웹 검색<b>거치지 않음</b>
              기록 프록시로, 트래픽 캡처와 독립적입니다.
              <br />
              소스 선택 가능 <b>DuckDuckGo(ddgs)</b>(불필요 Key),<b>Brave(무료 버전)</b>(입력 필요 Brave API Key),{" "}
              <b>Tavily</b>(입력 필요 Tavily API Key) 또는 <b>DeepSeek</b>(현재 재사용 LLM 설정). 메인 스위치가 꺼져 있으면 각
              Agent 의 웹 검색 스위치는 사용할 수 없습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="web-search" className="text-sm font-normal text-muted-foreground">
                {webSearch ? "메인 스위치 켜짐 · 각 Agent 설정에서 개별적으로 사용" : "꺼짐 · 각 Agent 웹 검색을 사용할 수 없음"}
              </Label>
              <Switch
                id="web-search"
                checked={webSearch}
                disabled={!loaded || saving}
                onCheckedChange={(v) => {
                  setWebSearch(v); // optimistic
                  saveWebSearch({ web_search_enabled: v });
                }}
              />
            </div>

            {webSearch && (
              <div className="flex items-center justify-between gap-4">
                <Label className="text-sm font-normal text-muted-foreground">검색 소스</Label>
                <Select
                  value={backend}
                  disabled={!loaded || saving}
                  onValueChange={(v) => {
                    setBackend(v); // optimistic
                    saveWebSearch({ web_search_backend: v });
                  }}
                >
                  <SelectTrigger className="w-48 shrink-0">
                    <SelectValue placeholder="소스 선택" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ddgs">DuckDuckGo(ddgs · 무료, 불필요: Key)</SelectItem>
                    <SelectItem value="brave-free">Brave(무료 버전 · 필요 Key)</SelectItem>
                    <SelectItem value="tavily">Tavily(필요: Key)</SelectItem>
                    <SelectItem value="deepseek">DeepSeek(공식)</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            )}

            {webSearch && backend === "deepseek" && (
              <div className="border-border/60 bg-muted/30 flex flex-col gap-2 rounded-md border p-3">
                <p className="text-sm font-medium">DeepSeek 공식 온라인 검색</p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  이 소스는 직접 재사용<b>현재 활성 LLM 설정</b>. 따라서 이것은
                  <b>지원: DeepSeek 공식 모델</b>, 그리고 이 설정<b>반드시 사용 anthropic 프로토콜</b>
                  ——DeepSeek 의 OpenAI 프로토콜 엔드포인트는 서버측 검색을 지원하지 않습니다. 전환 LLM 설정 후 이 소스가 무효화될 수 있습니다.
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  다른 소스와 달리, 검색은 <b>DeepSeek 서버측 실행</b>동작이거나 안정성에 영향을 줄 수 있으니, 영향을 이해한 뒤 사용하세요. Token
                  비용), 검색 요청<b>위의 출구 프록시를 거치지 않습니다</b>, 또한<b>트래픽 흔적에 포함되지 않음</b>; 반환 결과<b>제목과 링크만</b>
                  (요약 없음), 본문이 필요하면 WebFetch 가져옵니다.
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  위 조건 충족 여부는 직접 확인하며, 시스템은 차단하지 않습니다; 아래 「검색 테스트」 버튼으로 실제로 한 번 실행해 검증할 수 있습니다.
                </p>
              </div>
            )}

            {webSearch && backend === "brave-free" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="brave-key" className="text-sm font-normal text-muted-foreground">
                  Brave Search API Key
                  {braveKeySet && <span className="ml-2 text-xs text-emerald-500">설정됨</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="brave-key"
                    type="password"
                    autoComplete="off"
                    placeholder={braveKeySet ? "설정됨(비워 두면 유지)" : "입력 Brave API Key"}
                    value={braveKeyInput}
                    disabled={!loaded || savingKey}
                    onChange={(e) => setBraveKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveBraveKey}
                    disabled={!loaded || savingKey || braveKeyInput.trim() === ""}
                  >
                    저장
                  </Button>
                </div>
                {braveNeedsKey && (
                  <p className="text-xs text-amber-500">
                    선택됨 Brave 단 아직 설정되지 않음 Key —— 저장 시 Key 전에는 검색 도구가 사용되지 않습니다.
                  </p>
                )}
                <p className="text-muted-foreground text-xs">
                  무료 버전 할당량 약 2,000 회/월. 이동: https://brave.com/search/api/ 획득 Key.
                </p>
              </div>
            )}

            {webSearch && backend === "tavily" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="tavily-key" className="text-sm font-normal text-muted-foreground">
                  Tavily Search API Key
                  {tavilyKeySet && <span className="ml-2 text-xs text-emerald-500">설정됨</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="tavily-key"
                    type="password"
                    autoComplete="off"
                    placeholder={tavilyKeySet ? "설정됨(비워 두면 유지)" : "입력 Tavily API Key(tvly-…)"}
                    value={tavilyKeyInput}
                    disabled={!loaded || savingTavilyKey}
                    onChange={(e) => setTavilyKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveTavilyKey}
                    disabled={!loaded || savingTavilyKey || tavilyKeyInput.trim() === ""}
                  >
                    저장
                  </Button>
                </div>
                {webSearch && backend === "tavily" && !tavilyKeySet && (
                  <p className="text-xs text-amber-500">
                    선택됨 Tavily 단 아직 설정되지 않음 Key —— 저장 시 Key 전에는 검색 도구가 사용되지 않습니다.
                  </p>
                )}
                <p className="text-muted-foreground text-xs">이동 https://tavily.com 등록 후 획득 API Key.</p>
              </div>
            )}

            {webSearch && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="ws-proxy" className="text-sm font-normal text-muted-foreground">
                  출구 프록시(선택)
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="ws-proxy"
                    autoComplete="off"
                    placeholder="http://host:port 또는 socks5://host:port(비워 두면=직접 연결)"
                    value={proxyInput}
                    disabled={!loaded || savingProxy}
                    onChange={(e) => setProxyInput(e.target.value)}
                  />
                  <Button type="button" onClick={saveProxy} disabled={!loaded || savingProxy}>
                    저장
                  </Button>
                </div>
                <p className="text-muted-foreground text-xs">
                  독립 출구 프록시로, 검색 엔드포인트 접근에만 사용(VPN/SOCKS 등). 트래픽을 기록하는 MITM 프록시와 무관; 네트워크가 안 될 때 이 프록시를 거쳐 접근합니다.
                </p>
              </div>
            )}

            {webSearch && (
              <div className="flex items-center justify-between gap-4 border-t pt-4">
                <p className="text-muted-foreground text-xs">
                  현재 설정 사용(소스 + 프록시 + Key)로 실제로 한 번 검색: 「test」, 사용 가능 여부를 검증합니다.
                </p>
                <Button
                  type="button"
                  variant="outline"
                  onClick={runTest}
                  disabled={!loaded || testing}
                  className="shrink-0"
                >
                  {testing ? "테스트 중…" : "검색 테스트"}
                </Button>
              </div>
            )}
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              사용자 지정 스크립트 · Python 인터프리터
            </CardTitle>
            <CardDescription>
              사용자 지정 <b>script</b> 유형 도구가 이것으로 실행 Python. 부팅 시 자동 감지(python3 우선); 여기서 수동 입력 가능 venv /
              특정 버전의 절대 경로이며, 비워 두면 런타임에 자동 감지합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                className="font-mono text-sm"
                placeholder="/usr/bin/python3(비워 두면=자동 감지)"
                value={pyInterp}
                disabled={!loaded || saving}
                onChange={(e) => setPyInterp(e.target.value)}
              />
              <Button variant="outline" onClick={detectPython} disabled={!loaded || saving}>
                다시 감지
              </Button>
              <Button onClick={savePython} disabled={!loaded || saving}>
                저장
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <CpuIcon className="size-4" />
              Worker 동시 실행 · Work Agent 수
            </CardTitle>
            <CardDescription>
              각 작업에서 동시에 실행되는 Worker agent 수량(기본 3). 값이 클수록 동시 탐지가 많아지고 소모도 커집니다. 수정 후
              <b>이후 시작되는 작업에 적용</b>, 실행 중인 작업은 영향받지 않습니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                type="number"
                min={1}
                className="w-32 font-mono text-sm"
                placeholder="3"
                value={workers}
                disabled={!loaded || savingWorkers}
                onChange={(e) => setWorkers(e.target.value)}
              />
              <Button onClick={saveWorkers} disabled={!loaded || savingWorkers}>
                저장
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyboardIcon className="size-4" />
              세션 입력란 전송 키
            </CardTitle>
            <CardDescription>
              대화 페이지와 작업 상세의 메인 Agent 세션 입력란이 이 설정을 공유하며, 선택하면 즉시 적용되고 저장이 필요 없습니다.
              <br />
              이 환경 설정<b>이 브라우저에만 존재</b>, 계정과 동기화되지 않으며, 브라우저를 바꾸거나 사이트 데이터를 지우면 다시 설정해야 합니다.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="chat-send-mode" className="text-sm font-normal text-muted-foreground">
              전송 방식
            </Label>
            <Select value={sendMode} onValueChange={(v) => setChatSendMode(v as ChatSendMode)}>
              <SelectTrigger id="chat-send-mode" className="w-72">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {CHAT_SEND_MODE_OPTIONS.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
