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

  // 약관을 끝까지 스크롤해야(스크롤 없이 전체가 보이는 경우 포함) '동의'를 누를 수 있다.
  function handleTermsScroll() {
    const el = termsBodyRef.current;
    if (!el) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 8) setReadToEnd(true);
  }

  useEffect(() => {
    if (!termsOpen) return;
    // 열 때 초기화하고, 내용이 한 화면보다 짧아 스크롤이 일어나지 않는 경우도 처리한다.
    setReadToEnd(false);
    const el = termsBodyRef.current;
    if (el && el.scrollHeight <= el.clientHeight + 8) setReadToEnd(true);
  }, [termsOpen]);

  useEffect(() => {
    // 이미 로그인했으면 바로 메인 화면으로 간다(정적 export에는 이 이동을 대신할 middleware가 없다).
    const token = auth.getToken();
    if (token) {
      // localStorage에는 자격 증명이 남아 있지만 cookie는 사라졌을 수 있다. 먼저 맞춘 뒤 새 요청을 보내,
      // 서버 쪽 가드나 라우트 캐시가 아직 checking 상태인 로그인 페이지로 되돌려 보내지 않게 한다.
      auth.setToken(token);
      window.location.replace("/function/tasks");
      return;
    }
    api
      .authStatus()
      .then(({ initialized }) => {
        if (!initialized) router.replace("/setup");
      })
      .catch(() => setError("백엔드 서비스에 연결하지 못했습니다"))
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!agreed) {
      setError("먼저 '이용 안내'를 읽고 동의하세요");
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
            <p className="mx-auto max-w-xl text-muted-foreground">ARTEX를 계속 사용하려면 비밀번호를 입력하세요</p>
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
                placeholder="비밀번호"
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
                다음 문서를 읽었으며 동의합니다:
                <button
                  type="button"
                  onClick={() => setTermsOpen(true)}
                  className="mx-0.5 font-medium text-primary underline-offset-4 hover:underline"
                >
                  '이용 안내'
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
              <DialogTitle className="text-base">ARTEX 이용 안내 및 면책 조항</DialogTitle>
              <p className="text-xs text-muted-foreground">
                버전 v1.0 · 시행일 2026-09-18 · 로그인하기 전에 아래 약관을 모두 읽으세요
              </p>
            </div>
          </DialogHeader>

          <div
            ref={termsBodyRef}
            onScroll={handleTermsScroll}
            className="max-h-[60vh] space-y-5 overflow-y-auto px-6 py-5 text-sm leading-relaxed text-muted-foreground"
          >
            <p className="rounded-lg border bg-muted/40 p-3 text-foreground/80">
              이 '이용 안내 및 면책 조항'(이하 '이 조항')은 이 소프트웨어의 사용에 관해 귀하와 ARTEX 프로젝트 작성자 및
              기여자 사이에 맺는 약정입니다. 사용하기 전에 각 조항을 신중히 읽고 충분히 이해하세요. 특히 굵은 글씨나
              색으로 표시한 면책, 책임 제한, 금지 조항을 확인하세요.
              <span className="font-medium text-foreground">
                {" "}
                이 소프트웨어를 내려받거나 설치하거나 접근하거나 어떤 방식으로든 사용하면, 이 조항을 읽고 이해했으며 그
                전부에 동의한 것으로 봅니다.
              </span>
            </p>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  1
                </span>
                제1조 · 정의와 오픈 소스 라이선스
              </h4>
              <p className="pl-7">
                이 소프트웨어(ARTEX)는 GNU Affero General Public License v3.0(AGPL-3.0)으로 배포하는 오픈 소스
                프로그램입니다. 귀하는 이 라이선스에 따라 이 소프트웨어를 자유롭게 사용, 복제, 수정, 배포할 수 있습니다.
                다만 모든 파생 저작물(네트워크를 통해 제3자에게 제공하는 온라인 서비스 포함)은 똑같이 AGPL-3.0
                라이선스로 공개하고 사용자에게 해당 소스 코드 전체를 공개해야 합니다. AGPL-3.0의 전체 조항은 함께
                제공하는 LICENSE 파일을 따릅니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  2
                </span>
                제2조 · 허가된 사용 범위
              </h4>
              <p className="pl-7">
                이 소프트웨어는 개인 학습, 코드 연구, 보안 기술 원리 탐구, 그리고 귀하가 직접 구축한 격리된 로컬
                환경에서의 기술 검증에만 사용할 수 있으며, 학습·학술 연구·코드 검토처럼 공격적이지 않고 파괴적이지 않은
                용도에 맞습니다. 이 조에서 명시적으로 허가한 경우 외에는 이 소프트웨어를 다른 어떤 목적으로도 사용할 수
                없습니다.
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
                  어떤 웹사이트, 온라인 서비스, 타인 또는 제3자가 소유한 네트워크 연결 시스템에도 스캔, 탐지,
                  익스플로잇, 공격을 해서는 안 됩니다(허가를 받았는지, 귀하 소유의 자산인지와 관계없습니다).
                </li>
                <li>
                  이 소프트웨어를 실제 침투 테스트, 공방 대결, 레드팀·블루팀 훈련, 운영 환경에 사용해서는 안 됩니다.
                </li>
                <li>
                  이 소프트웨어를 불법 침입, 데이터 탈취, 금품 갈취, 서비스 거부(DoS/DDoS) 등 파괴적이거나 범죄적인
                  활동에 사용해서는 안 됩니다.
                </li>
                <li>
                  이 소프트웨어와 그 출력에 담긴 저작권, 라이선스, 보안 안내 정보를 제거하거나 변조하거나 회피해서는 안
                  됩니다.
                </li>
                <li>귀하가 있는 국가나 지역의 법률, 법규, 감독 규정을 위반하는 어떤 행위도 해서는 안 됩니다.</li>
              </ul>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  4
                </span>
                제4조 · 지식 재산권
              </h4>
              <p className="pl-7">
                이 소프트웨어의 저작권과 관련 지식 재산권은 프로젝트 작성자와 기여자에게 있으며, AGPL-3.0 라이선스가
                정한 범위 안에서 귀하에게 해당 권리를 부여합니다. 이 라이선스가 명시적으로 부여한 권리 외에, 이 조항은
                명시적으로든 묵시적으로든 귀하에게 다른 어떤 권리도 부여하지 않습니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  5
                </span>
                제5조 · 데이터와 개인정보
              </h4>
              <p className="pl-7">
                이 소프트웨어는 직접 배포해 쓰는 오픈 소스 프로그램이며, 작성자는 중앙 집중식 서비스를 운영하지 않고
                귀하의 사용 데이터를 수집하거나 업로드하지 않습니다. 사용 중에 만들거나 처리하거나 접하는 모든 데이터는
                귀하가 직접 관리하며 그 적법성과 보안에 책임을 집니다. 데이터를 부적절하게 처리해 생긴 모든 결과는
                귀하가 부담합니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  6
                </span>
                제6조 · 법규 준수와 법적 책임
              </h4>
              <p className="pl-7">
                귀하가 있는 국가나 지역의 네트워크 보안, 데이터 보안과 개인정보 보호, 컴퓨터 범죄 등에 관한 모든 법령을
                스스로 지켜야 합니다(중국 본토에서는 '사이버보안법', '데이터보안법', '개인정보보호법'과 관련 사법 해석을
                포함하며 이에 한정하지 않습니다).
                <span className="font-medium text-foreground">
                  {" "}
                  귀하가 위 법령이나 이 조항을 위반해 생긴 모든 법적 책임과 결과는 귀하 본인이 단독으로 부담하며, 이
                  소프트웨어의 작성자 및 기여자와는 관계가 없습니다.
                </span>
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  7
                </span>
                제7조 · 면책과 책임 제한
              </h4>
              <p className="pl-7">
                이 소프트웨어는 '있는 그대로(AS IS)'와 '제공 가능한 상태 그대로(AS AVAILABLE)' 제공하며, 상품성, 특정
                목적 적합성, 정확성, 권리 비침해에 대한 보증을 포함해 명시적이거나 묵시적인 어떤 보증도 하지 않습니다.
                관련 법률이 허용하는 최대 범위에서, 이 소프트웨어의 작성자와 기여자는 이 소프트웨어를 사용하거나
                사용하지 못해서(사용 방식이 적절했는지와 관계없이) 생긴 직접·간접·우발적·특수·결과적 손해에 책임을 지지
                않습니다. 이 손해에는 데이터 손실, 시스템 손상, 업무 중단, 이익 손실, 법적 분쟁이 포함되며 이에 한정하지
                않습니다.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  8
                </span>
                제8조 · 조항 변경과 최종 해석
              </h4>
              <p className="pl-7">
                작성자는 법령이나 프로젝트 발전에 따라 이 조항을 수시로 고칠 수 있으며, 고친 버전은 프로젝트와 함께
                배포되고 공개한 날부터 적용됩니다. 이 소프트웨어를 계속 사용하면 고친 조항을 받아들인 것으로 봅니다.
                법이 허용하는 범위에서 이 조항의 최종 해석권은 프로젝트 작성자에게 있습니다. 이 조항의 어느 한 조가
                무효로 판단되더라도 나머지 조항의 효력에는 영향을 주지 않습니다.
              </p>
            </section>
          </div>

          <DialogFooter className="mx-0 mb-0 flex-col items-stretch gap-2 rounded-b-xl px-6 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-xs text-muted-foreground">
              {readToEnd ? "약관을 모두 읽었습니다" : "약관을 끝까지 스크롤한 뒤 확인하세요"}
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
                모든 약관을 읽었으며 동의합니다
              </Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
