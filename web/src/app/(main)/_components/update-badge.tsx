"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * 상단 바의 "새 버전 있음" 안내: 전체 페이지 로드 시 한 번 조회하고, 업데이트가 있으면 버전 번호 옆에 표시하며,
 * 클릭하면 시스템 설정 페이지의 「버전과 업데이트」 카드로 바로 이동한다.
 *
 * 백엔드는 GitHub 조회 결과를 30분간 캐시하므로, 여기서 마운트마다 한 번 조회하는 것은 안전하다
 * —— 미인증 GitHub API는 60회/시간/IP뿐이라, 그 캐시가 없으면 탭을 여러 개만 열어도
 * 할당량을 소진해, 이후 정말 업데이트하려 할 때 조회가 안 된다.
 *
 * 조회 실패는 전부 무음 처리: 상단 바는 오류를 띄울 곳이 아니며, 사용자가 설정 페이지에서 「업데이트 확인」을 누르면 원인을 볼 수 있다.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update는 이미 "버전 번호 비교 가능" 판정을 포함하므로, 개발 빌드에서는 이 안내가 뜨지 않는다.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 무음 처리: 네트워크 없음 / GitHub 레이트 리밋 모두 상단 바에 오류를 띄워서는 안 된다.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`새 버전 ${latest} 발견, 클릭해 업데이트로 이동`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 숨 쉬는 점: 상단 바에 요소가 많아 순수 텍스트는 놓치기 쉬우므로, 애니메이션으로 한눈에 보이게 한다. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">새 버전 {latest}</span>
      <span className="sm:hidden">새 버전</span>
    </Link>
  );
}
