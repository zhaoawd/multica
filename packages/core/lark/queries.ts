import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const larkKeys = {
  all: (wsId: string) => ["lark", wsId] as const,
  binding: (wsId: string) => [...larkKeys.all(wsId), "binding"] as const,
  // User-scoped link — not workspace-keyed because the link lives on the
  // user, not the workspace. Kept under the same `lark` root for cache
  // sweep symmetry.
  userLink: () => ["lark", "user-link"] as const,
};

export const larkBindingOptions = (wsId: string) =>
  queryOptions({
    queryKey: larkKeys.binding(wsId),
    queryFn: () => api.getLarkBinding(wsId),
    enabled: !!wsId,
  });

export const larkUserLinkOptions = () =>
  queryOptions({
    queryKey: larkKeys.userLink(),
    queryFn: () => api.getMyLarkUserLink(),
  });
