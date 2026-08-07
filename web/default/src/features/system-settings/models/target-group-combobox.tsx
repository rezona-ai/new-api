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
import { useEffect, useMemo, useState } from 'react'
import { Check, ChevronsUpDown, Plus } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from '@/components/ui/command'
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from '@/components/ui/popover'

export type TargetGroupOption = {
  name: string
  ratio: number
  description: string
}

type TargetGroupComboboxProps = {
  value: string
  onValueChange: (value: string) => void
  options: TargetGroupOption[]
  disabled?: boolean
  placeholder?: string
}

/**
 * 目标分组选择器：既能搜索上方「分组定价」里已有的分组，也能直接手填一个定价表里
 * 还没有的分组名（后端只按名字匹配，允许先配倍率覆盖、后补定价行）。
 */
export function TargetGroupCombobox(props: TargetGroupComboboxProps) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [searchValue, setSearchValue] = useState('')

  useEffect(() => {
    if (!open) setSearchValue('')
  }, [open])

  const filteredOptions = useMemo(() => {
    const search = searchValue.trim().toLowerCase()
    if (!search) return props.options

    return props.options.filter(
      (option) =>
        option.name.toLowerCase().includes(search) ||
        option.description.toLowerCase().includes(search)
    )
  }, [props.options, searchValue])

  const trimmedSearch = searchValue.trim()
  const canCreate =
    trimmedSearch.length > 0 &&
    !props.options.some((option) => option.name === trimmedSearch)

  const handleSelect = (selectedValue: string) => {
    props.onValueChange(selectedValue)
    setOpen(false)
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <Button
            type='button'
            variant='outline'
            role='combobox'
            aria-expanded={open}
            disabled={props.disabled}
            className='w-full justify-between font-normal'
          />
        }
      >
        <span
          className={cn('truncate', !props.value && 'text-muted-foreground')}
        >
          {props.value || props.placeholder || t('Search or enter a group...')}
        </span>
        <ChevronsUpDown className='h-4 w-4 shrink-0 opacity-50' />
      </PopoverTrigger>
      <PopoverContent className='w-[var(--anchor-width)] p-0'>
        <Command shouldFilter={false}>
          <CommandInput
            placeholder={t('Search or enter a group...')}
            value={searchValue}
            onValueChange={setSearchValue}
          />
          <CommandList className='max-h-64'>
            {!canCreate && <CommandEmpty>{t('No group found.')}</CommandEmpty>}
            {canCreate && (
              <CommandGroup>
                <CommandItem
                  value={trimmedSearch}
                  onSelect={() => handleSelect(trimmedSearch)}
                >
                  <Plus className='h-4 w-4' />
                  <span className='truncate'>
                    {t('Use "{{group}}"', { group: trimmedSearch })}
                  </span>
                </CommandItem>
              </CommandGroup>
            )}
            {filteredOptions.length > 0 && (
              <CommandGroup heading={t('Group Pricing')}>
                {filteredOptions.map((option) => (
                  <CommandItem
                    key={option.name}
                    value={option.name}
                    onSelect={() => handleSelect(option.name)}
                    className='items-start gap-2'
                  >
                    <Check
                      className={cn(
                        'mt-0.5 h-4 w-4',
                        props.value === option.name
                          ? 'opacity-100'
                          : 'opacity-0'
                      )}
                    />
                    <span className='min-w-0 flex-1'>
                      <span className='block truncate font-medium'>
                        {option.name}
                      </span>
                      {option.description && (
                        <span className='text-muted-foreground block truncate text-xs'>
                          {option.description}
                        </span>
                      )}
                    </span>
                    <Badge variant='outline' className='shrink-0 text-xs'>
                      {option.ratio}x
                    </Badge>
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  )
}
