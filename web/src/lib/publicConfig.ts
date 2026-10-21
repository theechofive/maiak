import { useQuery } from "@tanstack/react-query";
import { getPublicConfig, queryKeys } from "../api/endpoints";

/**
 * Whether self-service registration is offered: undefined while the public config loads or when
 * it cannot be loaded, so callers show registration links only on an explicit true.
 */
export function useRegistrationEnabled(): boolean | undefined {
  const config = useQuery({
    queryKey: queryKeys.publicConfig,
    queryFn: getPublicConfig,
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
  });
  return config.data?.registrationEnabled;
}
