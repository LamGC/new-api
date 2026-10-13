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
import { useFormContext, useWatch } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Switch } from '@/components/ui/switch'

import type { ChannelFormValues } from '../lib/channel-form'

export function ResponsesViaChatSettings(props: {
  channelType: number
  disabled?: boolean
}) {
  const { t } = useTranslation()
  const form = useFormContext<ChannelFormValues>()
  const [bridge, passthrough, websocket] = useWatch({
    control: form.control,
    name: [
      'responses_via_chat',
      'pass_through_body_enabled',
      'responses_websocket_enabled',
    ],
  })
  const supported = [1, 3, 43].includes(props.channelType)
  const settings = [
    {
      name: 'responses_via_chat' as const,
      label: t('Enable Responses via Chat'),
      description: t(
        'Convert Responses requests to Chat Completions. Disabled by default; requires OpenAI, Azure or DeepSeek.'
      ),
      disabled:
        props.disabled || (!bridge && Boolean(passthrough || websocket)),
      visible: supported || bridge,
    },
    {
      name: 'responses_chat_compatibility' as const,
      label: t('Legacy Chat compatibility'),
      description: t(
        'Enable only for upstreams that require legacy Chat request compatibility.'
      ),
      disabled: props.disabled || !bridge,
      visible: supported || bridge,
    },
    {
      name: 'responses_local_replay' as const,
      label: t('Enable local Responses replay'),
      description: t(
        'Opt in to storing conversation content on this gateway for previous_response_id replay. Disabled by default.'
      ),
      disabled: props.disabled || !bridge,
      visible: supported || bridge,
    },
    {
      name: 'preserve_history_thinking' as const,
      label: t('Preserve history thinking'),
      description: t(
        'Preserve reasoning in conversation history independently of Responses via Chat. Unsigned Claude thinking is suitable only for compatible upstreams.'
      ),
      disabled: props.disabled,
      visible: true,
    },
  ]

  return (
    <>
      {settings
        .filter((setting) => setting.visible)
        .map((setting) => (
          <FormField
            key={setting.name}
            control={form.control}
            name={setting.name}
            render={({ field }) => (
              <FormItem className='flex items-center justify-between gap-4 px-4 py-3'>
                <div className='space-y-0.5'>
                  <FormLabel>{setting.label}</FormLabel>
                  <FormDescription>
                    {setting.description}
                    {setting.name === 'responses_via_chat' &&
                      (passthrough || websocket) && (
                        <>
                          {' '}
                          {t(
                            'Disable Pass Through Body and Responses WebSocket before enabling Responses via Chat'
                          )}
                        </>
                      )}
                  </FormDescription>
                  <FormMessage />
                </div>
                <FormControl>
                  <Switch
                    checked={
                      setting.name === 'responses_chat_compatibility'
                        ? field.value === 'legacy'
                        : field.value === true
                    }
                    onCheckedChange={(checked) => {
                      if (setting.name === 'responses_chat_compatibility') {
                        field.onChange(checked ? 'legacy' : '')
                        return
                      }
                      field.onChange(checked)
                      if (setting.name === 'responses_via_chat' && !checked) {
                        form.setValue('responses_chat_compatibility', '', {
                          shouldDirty: true,
                        })
                        form.setValue('responses_local_replay', false, {
                          shouldDirty: true,
                        })
                      }
                    }}
                    disabled={setting.disabled}
                    onBlur={field.onBlur}
                    name={field.name}
                    ref={field.ref}
                  />
                </FormControl>
              </FormItem>
            )}
          />
        ))}
    </>
  )
}
