import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const larkKeys = {
  all: (wsId: string) => ["lark", wsId] as const,
  binding: (wsId: string) => [...larkKeys.all(wsId), "binding"] as const,
};

export const larkBindingOptions = (wsId: string) =>
  queryOptions({
    queryKey: larkKeys.binding(wsId),
    queryFn: () => api.getLarkBinding(wsId),
    enabled: !!wsId,
  });
