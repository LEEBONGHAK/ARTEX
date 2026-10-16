"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** 새 버전이 올라오기를 기다리는 최대 시간. 한 번의 업그레이드는 세 번의 프로세스 시작을 거칩니다(스테이징 → 교체 → 새 버전),
 *  매번 초 단위이며, 3분이면 느린 디스크와 Docker 컨테이너 재구축. */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // 와 progress 분리: 스테이징 완료 후 프로세스가 사라지므로,SSE 끊기므로, 이때 폴링으로 전환해야 함 /api/health.
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet 동시에 백엔드 캐시를 우회할지 결정합니다: 페이지 진입 시 자동 확인은 캐시 사용(상단 바에서 방금 조회함),
  // 사용자가 수동으로 「업데이트 확인」을 누르면 강제로 원본을 조회하며, 그러지 않으면 방금 배포된 버전은 캐시가 만료되어야 보입니다.
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("업데이트 확인 실패:" + r.error);
          else if (r.has_update) toast.success(`새 버전 발견 ${r.latest}`);
          else if (r.comparable) toast.success("현재 이미 최신 버전");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("업데이트 확인 실패:" + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // 폴링 /api/health 버전 번호가 바뀔 때까지.
  //
  // 판단 기준은 반드시"버전이 바뀜"가 아니라"연결됨": 교체 과정에서 이전 버전이 잠시 다시 올라옵니다
  // (그 한 번은 다음만 담당: artex.new 교체한 뒤 즉시 종료), 연결성만 보면 성공으로 오판합니다.
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`다음으로 업데이트됨: ${j.version}, 페이지를 다시 로드하는 중`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // 재시작 윈도우 내 연결 불가는 예상된 것이므로 폴링을 계속합니다.
      }
    }
    setRestarting(false);
    toast.error("서비스 재시작 대기 타임아웃. 백엔드 로그를 확인하거나, 다음을 확인: artex 는 다음을 통해 start.sh / start.bat 로 시작됨.");
  }, []);

  // 업데이트 진행도 구독.SSE 거치지 않음 Next 의 /api 재작성(그 계층이 버퍼링하여 이벤트가 전달되지 않음).
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("업데이트 실패:" + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // 프로세스 종료 시 SSE 반드시 끊깁니다. 이미 재시작 대기에 진입했다면 이는 정상적인 현상이며,
        // 맡김 /api/health 폴링으로 계속 판정하면 됩니다.
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `다음으로 업데이트 확정: ${info.latest}?\n\n` +
        "업데이트는 프로그램을 재시작하며, 실행 중인 작업이 중단됩니다.\n" +
        (info.mode === "docker"
          ? "\n주의: 컨테이너 내 업데이트는 프로그램 자체만 교체하며, 이미지 안의 playwright / nmap 등 툴체인;" +
            "새 버전이 새 도구에 의존하면 다음으로 변경: docker compose pull."
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "준비 중…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("업데이트 시작 실패:" + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "이전 버전으로 롤백할까요?\n\n프로그램이 재시작되며, 실행 중인 작업이 중단됩니다.\n주의: 데이터베이스 구조는 되돌아가지 않으므로, 이전 버전은 새 버전이 쓴 데이터를 인식하지 못할 수 있습니다.",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("이전 버전으로 전환했으며 재시작 중…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("롤백 실패:" + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 다운로드 단계만 실제 퍼센트를 얻을 수 있음(기준: Content-Length 계산). 검증/압축 해제/재시작 대기는 모두
  // 소요 시간을 알 수 없는 단계로, 진행 막대를 채우고 펄스 애니메이션을 추가해 표시"작업 중이지만 얼마나 더 걸릴지 알 수 없음".
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // 설정 페이지는 다중 열 메이슨리 레이아웃이며, 카드가 직접 행 간격을 담당하고 열 간 분리를 금지합니다(참조: page.tsx 의 주석).
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          버전과 업데이트
        </CardTitle>
        <CardDescription>부터 GitHub 새 버전을 확인하고 설치합니다. 업데이트는 프로그램을 재시작하며, 실행 중인 작업이 중단됩니다.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">현재 버전</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "독립 프로그램"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">최신 버전</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              변경 로그 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            연결할 수 없음 GitHub:{info.error}
            {"　"}위에서 전역 프록시를 설정한 뒤 다시 시도할 수 있습니다.
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest} 제공되지 않음 {info.os}/{info.arch} 릴리스 패키지(누락: {info.asset}), 자동 업데이트할 수 없음.
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            다운로드함: <span className="font-mono">{info.asset}</span>
            {info.size ? `(${humanSize(info.size)})` : ""}, 검증 SHA256 스모크 테스트를 거친 뒤에야 교체하며, 실패 시 자동으로 현재 버전을 유지합니다.
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            현재 이미 최신 버전입니다.
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            Docker 환경의 업데이트는 프로그램 자체만 교체하고, 이미지 안의 것은 갱신하지 않음 playwright / nmap 등 툴체인, 그리고
            <span className="font-mono"> docker compose up -d </span>
            컨테이너를 재구축하면 이미지에 포함된 버전으로 되돌아갑니다. 이미지까지 함께 업그레이드하려면 다음을 실행:
            <span className="font-mono"> docker compose pull artex &amp;&amp; docker compose up -d artex</span>.
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting ? "재시작하여 새 버전을 적용하는 중입니다. 잠시 기다리세요(페이지가 자동으로 새로고침됩니다)…" : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            업데이트 확인
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `다음으로 업데이트: ${info.latest}` : "지금 업데이트"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              이전 버전으로 롤백
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          원클릭 업데이트는 감시 스크립트에 의존해 프로그램을 재시작합니다. 다음을 통해: <span className="font-mono">start.sh</span>(Windows 는
          <span className="font-mono"> start.bat</span>) 시작 ARTEX; 직접 실행 artex 본체일 때, 프로그램 종료 후 자동으로 다시 시작되지 않습니다.
        </p>
      </CardContent>
    </Card>
  );
}
