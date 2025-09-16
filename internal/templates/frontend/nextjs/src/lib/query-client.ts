import { createClient } from "@connectrpc/connect";
import type { DescService } from "@bufbuild/protobuf";
import { transport } from "./connect";

/**
 * connectClient creates a typed Connect client for a given service descriptor.
 *
 * Usage:
 *   import { UserService } from "../gen/user/v1/user_pb";
 *
 *   const client = connectClient(UserService);
 *   const user = await client.getUser({ id: "1" });
 */
export function connectClient<S extends DescService>(service: S) {
  return createClient(service, transport);
}
