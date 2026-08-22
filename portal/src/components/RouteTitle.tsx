import { useEffect } from "react";
import { useLocation } from "react-router";
import { titleFor } from "./routeTitleMap";

// Per-route document.title. Rendered once inside the router; updates on
// client-side navigation, which never re-reads index.html's static <title>.
export default function RouteTitle() {
  const { pathname } = useLocation();
  useEffect(() => {
    document.title = titleFor(pathname);
  }, [pathname]);
  return null;
}
