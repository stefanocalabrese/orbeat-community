import { useLocation } from "react-router";
import { useAuth } from "../auth/useAuth";
import HexLogo from "../components/HexLogo";
import { Button } from "../components/ui/Button";
import { Card } from "../components/ui/Card";

export default function LoginPage() {
  const { login } = useAuth();
  const location = useLocation();
  // Deep link the guard carried here (state.from); forwarded into login() so
  // the user lands back on the page they originally asked for after SSO.
  const from = (location.state as { from?: { pathname?: string } } | null)?.from?.pathname;
  return (
    <div className="grid min-h-screen place-items-center bg-bg px-4">
      <Card className="w-full max-w-sm p-8 text-center">
        <h1 className="flex items-center justify-center gap-2 text-xl font-semibold tracking-tight text-text">
          <HexLogo size={28} /> orbeat
        </h1>
        <p className="mt-2 text-sm text-muted">Governed catalog &amp; gateway for AI-agent capabilities</p>
        <Button variant="primary" className="mt-6 w-full" onClick={() => login(from)}>
          Sign in with SSO
        </Button>
      </Card>
    </div>
  );
}
