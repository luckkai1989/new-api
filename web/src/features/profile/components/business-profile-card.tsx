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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation } from '@tanstack/react-query'
import { Building2 } from 'lucide-react'
import { useEffect, useMemo } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import type { z } from 'zod'

import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { TitledCard } from '@/components/ui/titled-card'
import { handleServerError } from '@/lib/handle-server-error'
import { requireServerSuccess } from '@/lib/server-error-message'

import { updateBusinessProfile } from '../api'
import { getBusinessProfileSchema } from '../lib/business-profile'

interface BusinessProfileCardProps {
  businessSystemId: string
  onUpdate: () => void
}

export function BusinessProfileCard(props: BusinessProfileCardProps) {
  const { t } = useTranslation()
  const schema = useMemo(() => getBusinessProfileSchema(t), [t])
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: { business_system_id: props.businessSystemId },
  })
  useEffect(() => {
    form.reset({ business_system_id: props.businessSystemId })
  }, [form, props.businessSystemId])
  const update = useMutation({
    mutationFn: async (value: string) =>
      requireServerSuccess(await updateBusinessProfile(value)),
    onSuccess: (_result, value) => {
      form.reset({ business_system_id: value })
      toast.success(t('Business profile updated'))
      props.onUpdate()
    },
    onError: (error) =>
      handleServerError(error, t('Failed to update business profile')),
  })

  return (
    <TitledCard
      title={t('Business account')}
      icon={<Building2 className='size-4' />}
      iconTone='info'
      disableHoverEffect
    >
      <Form {...form}>
        <form
          onSubmit={form.handleSubmit((values) =>
            update.mutate(values.business_system_id)
          )}
          className='space-y-4'
        >
          <FormField
            control={form.control}
            name='business_system_id'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Business system ID')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    placeholder={t('Optional')}
                    disabled={update.isPending}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Identifies this business account in future tasks and usage logs. Historical records keep their original ID.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
          <Button
            type='submit'
            disabled={update.isPending || !form.formState.isDirty}
          >
            {update.isPending ? t('Saving...') : t('Save changes')}
          </Button>
        </form>
      </Form>
    </TitledCard>
  )
}
