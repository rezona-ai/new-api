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
import { useState } from 'react'
import { Plus, Zap } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { ERROR_MESSAGES } from '../constants'
import { quickAddUser } from '../lib'
import { useUsers } from './users-provider'

export function UsersPrimaryButtons() {
  const { t } = useTranslation()
  const { setOpen, setCurrentRow, triggerRefresh } = useUsers()
  const [quickUsername, setQuickUsername] = useState('')
  const [isQuickAdding, setIsQuickAdding] = useState(false)

  const handleCreate = () => {
    setCurrentRow(null)
    setOpen('create')
  }

  const handleQuickAdd = async () => {
    const username = quickUsername.trim()
    if (!username) {
      toast.error(t('Please enter a username first'))
      return
    }

    setIsQuickAdding(true)
    try {
      const result = await quickAddUser(username)
      if (!result.created) {
        toast.error(result.message || t(ERROR_MESSAGES.CREATE_FAILED))
        return
      }

      toast.success(t('User created successfully'), {
        description: `${username} · ${result.password}`,
      })
      if (!result.quotaApplied) {
        toast.warning(t('User created, but failed to set the quota'))
      }
      setQuickUsername('')
      triggerRefresh()
    } catch (_error) {
      toast.error(t(ERROR_MESSAGES.UNEXPECTED))
    } finally {
      setIsQuickAdding(false)
    }
  }

  return (
    <div className='flex flex-wrap items-center gap-2'>
      <Button size='sm' onClick={handleCreate}>
        <Plus className='h-4 w-4' />
        {t('Add User')}
      </Button>
      <div className='flex items-center gap-2'>
        <Input
          value={quickUsername}
          onChange={(e) => setQuickUsername(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !isQuickAdding) handleQuickAdd()
          }}
          placeholder={t('Enter username')}
          className='h-8 w-40'
          disabled={isQuickAdding}
        />
        <Button
          size='sm'
          variant='outline'
          onClick={handleQuickAdd}
          disabled={isQuickAdding}
        >
          <Zap className='h-4 w-4' />
          {isQuickAdding ? t('Saving...') : t('Quick Add User')}
        </Button>
      </div>
    </div>
  )
}
