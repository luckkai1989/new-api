/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { AlertTriangle } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { toIntlLocale } from '@/i18n/languages'
import { formatTimestampToDate } from '@/lib/format'

import type { Channel, UpstreamMonitorPriceSummary } from '../types'
import { useChannels } from './channels-provider'

export function UpstreamModelRatioCell(props: {
  channel: Channel
  summary?: UpstreamMonitorPriceSummary
  loading?: boolean
  failed?: boolean
}) {
  const { t, i18n } = useTranslation()
  const { setOpen, setCurrentRow, sensitiveVisible } = useChannels()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const ratio = props.summary?.model_ratio
  let value = '-'
  if (props.loading && !props.summary) {
    value = '...'
  } else if (ratio != null && Number.isFinite(ratio) && ratio >= 0) {
    value = `${new Intl.NumberFormat(locale, { maximumFractionDigits: 8 }).format(ratio)}×`
  }
  const error = props.failed
    ? t('Price snapshot unavailable')
    : props.summary?.last_error
  const model = props.summary?.first_model || t('Not sampled')

  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            variant='ghost'
            className='h-12 w-full min-w-0 justify-start px-0 text-left'
            aria-label={t('Upstream model ratio')}
            onClick={() => {
              setCurrentRow(props.channel)
              setOpen('upstream-monitor')
            }}
          >
            <span className='min-w-0'>
              <span className='flex items-center gap-1 tabular-nums'>
                {sensitiveVisible ? value : '******'}
                {error && <AlertTriangle className='size-3 text-amber-600' />}
              </span>
              <span className='text-muted-foreground block truncate text-xs font-normal'>
                {model}
              </span>
            </span>
          </Button>
        }
      />
      <TooltipContent className='block space-y-1 wrap-anywhere'>
        <p>{model}</p>
        <p>
          {t('Last price sample: {{time}}', {
            time: props.summary?.last_price_at
              ? formatTimestampToDate(props.summary.last_price_at)
              : t('Not sampled'),
          })}
        </p>
        {props.summary && !props.summary.enabled && (
          <p>{t('Monitoring disabled')}</p>
        )}
        {error && <p>{error}</p>}
      </TooltipContent>
    </Tooltip>
  )
}
