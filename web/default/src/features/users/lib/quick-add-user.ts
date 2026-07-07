/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { parseQuotaFromDollars } from '@/lib/format'
import { createUser, searchUsers, adjustUserQuota } from '../api'

// ============================================================================
// Quick Add User
// ============================================================================
// One-click user creation for admins: generate a random password, create the
// user with that password (also used as the display name so the admin can read
// it back from the list), then grant a default quota. The backend CreateUser
// endpoint does not return the new id and does not accept a quota on create, so
// the quota is applied in a second step after resolving the id by username.

// 20 chars: the backend caps password AND display_name at 20 (validate:"max=20"),
// so the generated password (reused as display name) sits exactly at the limit.
export const QUICK_ADD_PASSWORD_LENGTH = 20
// Default quota granted to a quick-added user, in USD.
export const QUICK_ADD_QUOTA_DOLLARS = 10

// Alphanumeric only — avoids characters that are awkward to copy/paste or that
// might trip shell/URL handling when the admin hands the password to the user.
const PASSWORD_CHARSET =
  'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'

/**
 * Generate a cryptographically-random alphanumeric password.
 */
export function generateRandomPassword(
  length: number = QUICK_ADD_PASSWORD_LENGTH
): string {
  const values = new Uint32Array(length)
  crypto.getRandomValues(values)
  let result = ''
  for (let i = 0; i < length; i++) {
    result += PASSWORD_CHARSET[values[i] % PASSWORD_CHARSET.length]
  }
  return result
}

export type QuickAddUserResult = {
  created: boolean
  // The generated password (only present when the user was created).
  password?: string
  // Whether the default quota was applied after creation.
  quotaApplied: boolean
  // Error message from the failing step (create) when created === false.
  message?: string
}

/**
 * Create a common user in one shot: random 20-char password (also the display
 * name) + default quota. Returns the generated password so the caller can show
 * it to the admin.
 */
export async function quickAddUser(
  username: string
): Promise<QuickAddUserResult> {
  const password = generateRandomPassword()

  const created = await createUser({
    username,
    display_name: password,
    password,
    role: 1, // common user
  })
  if (!created.success) {
    return { created: false, quotaApplied: false, message: created.message }
  }

  // CreateUser returns no id, so resolve it by exact username to set the quota.
  let quotaApplied = false
  try {
    const list = await searchUsers({ keyword: username, page_size: 100 })
    const newUser = list.data?.items?.find((u) => u.username === username)
    if (newUser) {
      const quota = await adjustUserQuota({
        id: newUser.id,
        action: 'add_quota',
        mode: 'override',
        value: parseQuotaFromDollars(QUICK_ADD_QUOTA_DOLLARS),
      })
      quotaApplied = quota.success
    }
  } catch {
    quotaApplied = false
  }

  return { created: true, password, quotaApplied }
}
