"use client";

import { useEffect, useRef, useState } from "react";

import { useRouter } from "next/navigation";

import { AlertTriangle, ShieldCheck } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogClose, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import { auth } from "@/lib/auth";

export default function LoginPage() {
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [checking, setChecking] = useState(true);
  const [agreed, setAgreed] = useState(false);
  const [termsOpen, setTermsOpen] = useState(false);
  const [readToEnd, setReadToEnd] = useState(false);
  const termsBodyRef = useRef<HTMLDivElement>(null);

  // 약관 맨 아래까지 스크롤해야(스크롤 없이 전부 보이는 경우 포함) '동의'를 누를 수 있다.
  function handleTermsScroll() {
    const el = termsBodyRef.current;
    if (!el) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 8) setReadToEnd(true);
  }

  useEffect(() => {
    if (!termsOpen) return;
    // 열릴 때 초기화하고, 내용이 한 화면보다 짧아 스크롤이 발생하지 않는 경우를 처리한다.
    setReadToEnd(false);
    const el = termsBodyRef.current;
    if (el && el.scrollHeight <= el.clientHeight + 8) setReadToEnd(true);
  }, [termsOpen]);

  useEffect(() => {
    // 이미 로그인한 경우 메인 화면으로 바로 이동(정적 내보내기에서는 이 리다이렉트를 대신할 middleware가 없다).
    const token = auth.getToken();
    if (token) {
      // localStorage에는 자격 증명이 남아 있지만 cookie는 사라졌을 수 있다. 먼저 동기화한 뒤 새 요청을 보내,
      // 서버 가드나 라우트 캐시가 아직 checking 상태인 로그인 페이지로 리다이렉트를 되돌리는 것을 막는다.
      auth.setToken(token);
      window.location.replace("/function/tasks");
      return;
    }
    api
      .authStatus()
      .then(({ initialized }) => {
        if (!initialized) router.replace("/setup");
      })
      .catch(() => setError("백엔드 서비스에 연결할 수 없습니다"))
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!agreed) {
      setError("먼저 《이용 안내》를 읽고 동의해 주세요");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.login("ARTEX", password);
      auth.setToken(token);
      window.location.replace("/function/tasks");
    } catch {
      setError("사용자 이름 또는 비밀번호가 올바르지 않습니다");
    } finally {
      setLoading(false);
    }
  }

  if (checking) {
    return (
      <div role="status" className="flex min-h-dvh items-center justify-center text-muted-foreground">
        로그인 상태를 확인하는 중…
      </div>
    );
  }

  return (
    <div className="flex h-dvh">
      {/* Left panel */}
      <div className="hidden flex-col items-center justify-center bg-primary p-12 text-center lg:flex lg:w-1/3">
        <div className="relative flex items-center justify-center">
          <div className="absolute size-80 rounded-full border border-primary-foreground/10" />
          <div className="absolute size-60 rounded-full border border-primary-foreground/15" />
          <div className="absolute size-40 rounded-full border border-primary-foreground/20" />
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/logo.png" alt="ARTEX" width={160} height={160} className="relative brightness-0 invert" />
        </div>
      </div>

      {/* Right panel */}
      <div className="flex w-full items-center justify-center bg-background p-8 lg:w-2/3">
        <div className="w-full max-w-md space-y-10 py-24 lg:py-32">
          <div className="space-y-4 text-center">
            <h2 className="text-2xl font-medium tracking-tight">로그인</h2>
            <p className="mx-auto max-w-xl text-muted-foreground">다시 오신 것을 환영합니다. ARTEX를 계속 사용하려면 비밀번호를 입력하세요</p>
          </div>
          <form onSubmit={handleSubmit} className="flex flex-col gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="username">사용자 이름</Label>
              <Input id="username" value="ARTEX" readOnly className="bg-muted text-muted-foreground" />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="password">비밀번호</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="비밀번호를 입력하세요"
                autoFocus
                autoComplete="current-password"
              />
            </div>
            <div className="flex items-start gap-2">
              <Checkbox
                id="agree-terms"
                checked={agreed}
                onCheckedChange={(v) => setAgreed(v === true)}
                className="mt-0.5"
              />
              <Label htmlFor="agree-terms" className="text-sm font-normal leading-relaxed text-muted-foreground">
                다음을 읽고 동의합니다
                <button
                  type="button"
                  onClick={() => setTermsOpen(true)}
                  className="mx-0.5 font-medium text-primary underline-offset-4 hover:underline"
                >
                  《이용 안내》
                </button>
              </Label>
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button type="submit" className="w-full" disabled={loading || !password || !agreed}>
              {loading ? "로그인 중..." : "로그인"}
            </Button>
          </form>
        </div>
      </div>

      <Dialog open={termsOpen} onOpenChange={setTermsOpen}>
        <DialogContent className="gap-0 p-0 sm:max-w-2xl">
          <DialogHeader className="flex-row items-center gap-3 border-b px-6 py-4">
            <div className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <ShieldCheck className="size-5" />
            </div>
            <div className="space-y-0.5">
              <DialogTitle className="text-base">ARTEX 이용 안내 및 면책 고지</DialogTitle>
              <p className="text-xs text-muted-foreground">
                버전 v1.0 · 시행일 2026-09-18 · 로그인 전 아래 모든 조항을 끝까지 읽어 주세요
              </p>
            </div>
          </DialogHeader>

          <div
            ref={termsBodyRef}
            onScroll={handleTermsScroll}
            className="max-h-[60vh] space-y-5 overflow-y-auto px-6 py-5 text-sm leading-relaxed text-muted-foreground"
          >
            <p className="rounded-lg border bg-muted/40 p-3 text-foreground/80">
              본 《이용 안내 및 면책 고지》(이하 "본 고지")는 귀하와 ARTEX
              프로젝트 작성자 및 기여자 사이에 본 소프트웨어 사용에 관해 체결된 약정입니다. 사용 전 각 조항, 특히 굵은 글씨나 색상 블록으로 표시된 면책·책임 제한·금지 조항을 신중히 읽고 충분히 이해해 주시기 바랍니다.
              <span className="font-medium text-foreground">
                {" "}
                귀하가 본 소프트웨어를 다운로드·설치·접근하거나 어떤 방식으로든 사용하는 즉시, 본 고지의 모든 내용을 읽고 이해하였으며 그 구속을 받는 데 동의한 것으로 간주합니다.
              </span>
            </p>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  1
                </span>
                제1조 · 정의 및 오픈소스 라이선스
              </h4>
              <p className="pl-7">
                본 소프트웨어(ARTEX)는 GNU Affero General Public License
                v3.0(AGPL-3.0)에 따라 배포되는 오픈소스 프로그램입니다. 귀하는 해당 라이선스에 따라 본 소프트웨어를 자유롭게 사용·복제·수정·배포할 수 있습니다. 다만 모든 파생 저작물(네트워크를 통해 제3자에게 제공하는 온라인 서비스 포함)은 동일하게
                AGPL-3.0 라이선스로 공개하고 이용자에게 해당 전체 소스 코드를 제공해야 합니다. AGPL-3.0의 전체 조항은 함께 제공되는 LICENSE 파일을 따릅니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  2
                </span>
                제2조 · 허용 사용 범위
              </h4>
              <p className="pl-7">
                본 소프트웨어는 오직 개인 학습, 코드 연구, 보안 기술 원리 탐구, 그리고 귀하가 직접 구축한 로컬 격리 환경에서의 기술 검증 용도로만 제공되며, 학습·학술 연구·코드 리뷰 등 비공격적·비파괴적 용도에 적용됩니다. 본 조에서 명시적으로 허용한 경우를 제외하고, 귀하는 본 소프트웨어를 그 밖의 어떤 목적으로도 사용해서는 안 됩니다.
              </p>
            </section>

            <section className="space-y-2">
              <h4 className="flex items-center gap-2 font-medium text-destructive">
                <span className="flex size-5 items-center justify-center rounded-md bg-destructive/10 text-xs font-semibold text-destructive">
                  3
                </span>
                <AlertTriangle className="size-4" />
                제3조 · 금지 행위
              </h4>
              <ul className="ml-7 list-decimal space-y-1.5 rounded-lg border border-destructive/20 bg-destructive/5 p-3 pl-8 text-foreground/80 marker:text-destructive/70">
                <li>
                  어떤 웹사이트, 온라인 서비스, 타인 또는 제3자 소유의 네트워크 연결 시스템에 대해서도 스캔·탐지·악용·공격을 수행하는 행위를 엄격히 금지합니다(권한 취득 여부, 귀하 자신의 자산인지 여부와 무관).
                </li>
                <li>본 소프트웨어를 실제 침투 테스트, 공방 대결, 레드/블루 훈련 또는 운영 환경에 사용하는 행위를 엄격히 금지합니다.</li>
                <li>본 소프트웨어를 불법 침입, 데이터 탈취, 금품 요구, 서비스 거부(DoS/DDoS) 또는 그 밖의 파괴적·범죄적 활동에 사용하는 행위를 엄격히 금지합니다.</li>
                <li>본 소프트웨어 및 그 출력물에 포함된 저작권·라이선스·보안 고지 정보를 제거·변조·우회하는 행위를 엄격히 금지합니다.</li>
                <li>귀하가 속한 국가 또는 지역의 법률·법규 및 규제 규정을 위반하는 일체의 행위를 엄격히 금지합니다.</li>
              </ul>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  4
                </span>
                제4조 · 지식재산권
              </h4>
              <p className="pl-7">
                본 소프트웨어의 저작권 및 관련 지식재산권은 프로젝트 작성자 및 기여자에게 귀속되며, AGPL-3.0
                라이선스에서 정한 범위 내에서 귀하에게 해당 권리를 부여합니다. 해당 라이선스에서 명시적으로 부여한 권리를 제외하고, 본 고지는 명시적이든 묵시적이든 귀하에게 그 밖의 어떤 권리도 부여하지 않습니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  5
                </span>
                제5조 · 데이터 및 개인정보
              </h4>
              <p className="pl-7">
                본 소프트웨어는 자체 배포가 가능한 오픈소스 프로그램으로, 작성자는 어떤 중앙 집중식 서비스도 운영하지 않으며 귀하의 사용 데이터를 수집하거나 업로드하지 않습니다. 사용 과정에서 생성·처리·접근하는 모든 데이터는 귀하가 직접 통제하며 그 적법성과 안전성에 대한 책임을 집니다. 데이터 처리 부주의로 발생한 모든 결과는 귀하가 스스로 부담합니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  6
                </span>
                제6조 · 준법 및 법적 책임
              </h4>
              <p className="pl-7">
                귀하는 자신이 속한 국가 또는 지역의 네트워크 보안, 데이터 보안 및 개인정보 보호, 컴퓨터 범죄 등에 관한 모든 법률·법규를 스스로 준수해야 합니다.
                <span className="font-medium text-foreground">
                  {" "}
                  귀하가 위 법률·법규 또는 본 고지의 약정을 위반하여 발생한 모든 법적 책임과 결과는 전적으로 귀하 본인이 부담하며, 본 소프트웨어의 작성자 및 기여자와는 무관합니다.
                </span>
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  7
                </span>
                제7조 · 면책 고지 및 책임 제한
              </h4>
              <p className="pl-7">
                본 소프트웨어는 "있는 그대로(AS IS)" 및 "이용 가능한 상태(AS
                AVAILABLE)"로 제공되며, 상품성, 특정 목적 적합성, 정확성 및 비침해성에 대한 보증을 포함하되 이에 한정되지 않는 어떠한 명시적·묵시적 보증도 수반하지 않습니다. 관련 법률이 허용하는 최대 범위 내에서, 본 소프트웨어의 작성자 및 기여자는 본 소프트웨어의 사용 또는 사용 불능(사용 방식의 적절성 여부와 무관)으로 인해 발생한 직접·간접·우발적·특별·결과적 손해에 대해 데이터 손실, 시스템 손상, 업무 중단, 이익 손실 또는 법적 분쟁을 포함하되 이에 한정되지 않는 어떠한 책임도 지지 않습니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  8
                </span>
                제8조 · 조항 변경 및 최종 해석
              </h4>
              <p className="pl-7">
                작성자는 법률·법규 또는 프로젝트 발전상의 필요에 따라 본 고지를 수시로 갱신할 권리를 가지며, 갱신된 버전은 프로젝트와 함께 배포되어 공표일부터 효력이 발생합니다. 귀하가 본 소프트웨어를 계속 사용하는 것은 개정된 조항을 수락한 것으로 간주합니다. 법률이 허용하는 범위 내에서 본 고지의 최종 해석권은 프로젝트 작성자에게 있습니다. 본 고지의 어느 한 조항이 무효로 판단되더라도 나머지 조항의 효력에는 영향을 미치지 않습니다.
              </p>
            </section>
          </div>

          <DialogFooter className="mx-0 mb-0 flex-col items-stretch gap-2 rounded-b-xl px-6 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-xs text-muted-foreground">
              {readToEnd ? "모든 조항을 확인하셨습니다" : "조항을 끝까지 스크롤한 뒤 확인해 주세요"}
            </p>
            <DialogClose asChild>
              <Button
                type="button"
                disabled={!readToEnd}
                onClick={() => {
                  setAgreed(true);
                  setError("");
                }}
              >
                모든 조항을 읽고 동의합니다
              </Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
