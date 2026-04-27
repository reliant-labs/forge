import { useMutation, type UseMutationOptions } from "@tanstack/react-query";

/**
 * useApiMutation wraps a Connect client promise-returning call in a
 * `@tanstack/react-query` `useMutation`.
 *
 * This helper centralizes the mutation pattern so every RPC-backed
 * mutation hook looks the same.
 *
 * Example:
 *
 *   import { connectClient } from "@/lib/connect";
 *   import { UserService } from "@/gen/user/v1/user_pb";
 *
 *   const userClient = connectClient(UserService);
 *
 *   export function useDeleteUser() {
 *     return useApiMutation((req: { id: string }) =>
 *       userClient.deleteUser(req),
 *     );
 *   }
 */
export function useApiMutation<TData, TVariables, TError = Error>(
  mutationFn: (variables: TVariables) => Promise<TData>,
  options?: Omit<
    UseMutationOptions<TData, TError, TVariables>,
    "mutationFn"
  >,
) {
  return useMutation<TData, TError, TVariables>({
    mutationFn,
    ...options,
  });
}
