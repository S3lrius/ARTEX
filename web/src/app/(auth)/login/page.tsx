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

  // Agree stays disabled until the terms are scrolled to the bottom, including when they already fit without scrolling.
  function handleTermsScroll() {
    const el = termsBodyRef.current;
    if (!el) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - 8) setReadToEnd(true);
  }

  useEffect(() => {
    if (!termsOpen) return;
    // Reset on open, and treat content that already fits on one screen as fully read.
    setReadToEnd(false);
    const el = termsBodyRef.current;
    if (el && el.scrollHeight <= el.clientHeight + 8) setReadToEnd(true);
  }, [termsOpen]);

  useEffect(() => {
    // Already signed in: go straight to the app. Static export has no middleware to do this redirect.
    const token = auth.getToken();
    if (token) {
      // localStorage may still hold a credential after the cookie is gone. Sync it
      // first, then navigate fresh, so a server guard or route cache does not send
      // us back to the login page while it is still checking.
      auth.setToken(token);
      window.location.replace("/function/tasks");
      return;
    }
    api
      .authStatus()
      .then(({ initialized }) => {
        if (!initialized) router.replace("/setup");
      })
      .catch(() => setError("Couldn't reach the backend"))
      .finally(() => setChecking(false));
  }, [router]);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!agreed) {
      setError("Read and accept the Terms of use first");
      return;
    }
    setLoading(true);
    setError("");
    try {
      const { token } = await api.login("ARTEX", password);
      auth.setToken(token);
      window.location.replace("/function/tasks");
    } catch {
      setError("Incorrect username or password");
    } finally {
      setLoading(false);
    }
  }

  if (checking) {
    return (
      <div role="status" className="flex min-h-dvh items-center justify-center text-muted-foreground">
        Checking sign-in status…
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
            <h2 className="text-2xl font-medium tracking-tight">Sign in</h2>
            <p className="mx-auto max-w-xl text-muted-foreground">Welcome back. Enter your password to continue.</p>
          </div>
          <form onSubmit={handleSubmit} className="flex flex-col gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="username">Username</Label>
              <Input id="username" value="ARTEX" readOnly className="bg-muted text-muted-foreground" />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="password">Password</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="Enter your password"
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
                I have read and agree to the
                <button
                  type="button"
                  onClick={() => setTermsOpen(true)}
                  className="mx-0.5 font-medium text-primary underline-offset-4 hover:underline"
                >
                  Terms of use
                </button>
              </Label>
            </div>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <Button type="submit" className="w-full" disabled={loading || !password || !agreed}>
              {loading ? "Signing in..." : "Sign in"}
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
              <DialogTitle className="text-base">ARTEX terms of use and disclaimer</DialogTitle>
              <p className="text-xs text-muted-foreground">
                Version v1.0 · Effective 2026-09-18 · Read every clause before signing in
              </p>
            </div>
          </DialogHeader>

          <div
            ref={termsBodyRef}
            onScroll={handleTermsScroll}
            className="max-h-[60vh] space-y-5 overflow-y-auto px-6 py-5 text-sm leading-relaxed text-muted-foreground"
          >
            <p className="rounded-lg border bg-muted/40 p-3 text-foreground/80">
              These Terms of Use and Disclaimer ("these Terms") are the agreement between you and the ARTEX
              authors and contributors about your use of this software. Read them carefully before you use it, especially the clauses in bold or highlighted blocks that disclaim warranties, limit liability, or prohibit certain uses.
              <span className="font-medium text-foreground">
                {" "}
                By downloading, installing, accessing, or otherwise using this software, you confirm that you have read, understood, and agree to be bound by these Terms.
              </span>
            </p>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  1
                </span>
                Definitions and open-source license
              </h4>
              <p className="pl-7">
                This software (ARTEX) is released under the GNU Affero General Public License
                v3.0 (AGPL-3.0). You may use, copy, modify, and distribute it under that license. Any derivative work, including an online service offered to third parties over a network, must also be released under
                AGPL-3.0 and must make the corresponding complete source available to its users. The full AGPL-3.0 text is the LICENSE file shipped with the project.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  2
                </span>
                Permitted use
              </h4>
              <p className="pl-7">
                This software is for personal learning, source study, discussion of security concepts, and technical verification in a local isolated environment that you operate yourself. Suitable uses are learning, academic research, and code review — work that is neither offensive nor destructive. You may not use it for any other purpose.
              </p>
            </section>

            <section className="space-y-2">
              <h4 className="flex items-center gap-2 font-medium text-destructive">
                <span className="flex size-5 items-center justify-center rounded-md bg-destructive/10 text-xs font-semibold text-destructive">
                  3
                </span>
                <AlertTriangle className="size-4" />
                Prohibited conduct
              </h4>
              <ul className="ml-7 list-decimal space-y-1.5 rounded-lg border border-destructive/20 bg-destructive/5 p-3 pl-8 text-foreground/80 marker:text-destructive/70">
                <li>
                  Do not scan, probe, exploit, or attack any website, online service, or networked system owned by someone else, whether or not you believe you have authorization and whether or not you consider it your own asset.
                </li>
                <li>Do not use this software for live penetration tests, attack-and-defense exercises, red-team or blue-team drills, or production environments.</li>
                <li>Do not use this software for unauthorized access, data theft, extortion, denial of service (DoS/DDoS), or any other destructive or criminal activity.</li>
                <li>Do not remove, alter, or bypass copyright, license, or safety notices in this software or its output.</li>
                <li>Do not do anything that violates the laws or regulations of your country or region.</li>
              </ul>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  4
                </span>
                Intellectual property
              </h4>
              <p className="pl-7">
                Copyright and related rights in this software belong to the project authors and contributors. Rights are granted to you only as AGPL-3.0
                provides. These Terms grant no other rights, express or implied.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  5
                </span>
                Data and privacy
              </h4>
              <p className="pl-7">
                This software is self-hosted open source. The authors do not operate a central service and do not collect or upload your usage data. You control every piece of data you create, process, or encounter while using it, and you are responsible for its legality and security. You bear the consequences of mishandling that data.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  6
                </span>
                Compliance and legal responsibility
              </h4>
              <p className="pl-7">
                You must comply with the cybersecurity, data-security, personal-information, and computer-crime laws of your country or region. In mainland China that includes, without limitation, the Cybersecurity Law, the Data Security Law, the Personal Information Protection Law, and related judicial interpretations.
                <span className="font-medium text-foreground">
                  {" "}
                  You alone bear every legal consequence of violating those laws or these Terms. The authors and contributors of this software are not responsible.
                </span>
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  7
                </span>
                Disclaimer and limitation of liability
              </h4>
              <p className="pl-7">
                This software is provided "AS IS" and "AS
                AVAILABLE", without warranty of any kind, express or implied, including warranties of merchantability, fitness for a particular purpose, accuracy, and non-infringement. To the maximum extent permitted by law, the authors and contributors are not liable for any direct, indirect, incidental, special, or consequential loss arising from use of or inability to use this software, whether or not that use was proper, including data loss, system damage, business interruption, lost profits, or legal disputes.
              </p>
            </section>

            <section className="space-y-1.5">
              <h4 className="flex items-center gap-2 font-medium text-foreground">
                <span className="flex size-5 items-center justify-center rounded-md bg-muted text-xs font-semibold text-muted-foreground">
                  8
                </span>
                Changes and interpretation
              </h4>
              <p className="pl-7">
                The authors may update these Terms as the law or the project requires. The updated version ships with the project and takes effect when it is published. Continuing to use the software means you accept the revised terms. To the extent the law allows, the project authors have the final say on how these Terms are interpreted. If any clause is held invalid, the rest remain in effect.
              </p>
            </section>
          </div>

          <DialogFooter className="mx-0 mb-0 flex-col items-stretch gap-2 rounded-b-xl px-6 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-xs text-muted-foreground">
              {readToEnd ? "You have read all of the terms" : "Scroll to the bottom of the terms before confirming"}
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
                I have read and agree to all terms
              </Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
