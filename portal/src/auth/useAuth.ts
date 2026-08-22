import { createContext, useContext } from "react";

export interface AuthState {
  isLoading: boolean;
  authenticated: boolean;
  token: string;
  subject: string;
  email: string;
  roles: string[];
  /**
   * Start the OIDC signin redirect. `target` (an absolute same-origin path,
   * e.g. a guard-carried deep link) overrides the default post-login restore
   * target, which is the current path.
   */
  login: (target?: string) => void;
  logout: () => void;
}

export const AuthCtx = createContext<AuthState>({
  isLoading: true,
  authenticated: false,
  token: "",
  subject: "",
  email: "",
  roles: [],
  login: () => {},
  logout: () => {},
});

export const useAuth = () => useContext(AuthCtx);
export const useIsAdmin = () => useAuth().roles.includes("orbeat-admin");
