import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useForm } from 'react-hook-form'
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
import { assert, describe, expect, test } from 'vitest'

import { Form } from '@/components/ui/form'

import { CHANNEL_TYPE_NEW_API, CHANNEL_TYPE_SUB2API } from '../../constants'
import { CHANNEL_TYPE_ADVANCED_CUSTOM } from '../../lib/advanced-custom'
import {
  buildSettingJSON,
  channelFormSchema,
  CHANNEL_FORM_DEFAULT_VALUES,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
  type ChannelFormValues,
} from '../../lib/channel-form'
import { channelSchema } from '../../types'
import { ResponsesViaChatSettings } from '../responses-via-chat-settings'
import { ResponsesWebSocketSetting } from '../responses-websocket-setting'

const supportedChannelTypes = [
  1,
  57,
  CHANNEL_TYPE_ADVANCED_CUSTOM,
  CHANNEL_TYPE_SUB2API,
  CHANNEL_TYPE_NEW_API,
]

function SettingsForm(props: {
  channelType: number
  disabled?: boolean
  enabled?: boolean
  bridgeSettings?: boolean
  defaults?: Partial<ChannelFormValues>
  onSave: (value: string) => void
}) {
  const form = useForm<ChannelFormValues>({
    defaultValues: {
      ...CHANNEL_FORM_DEFAULT_VALUES,
      type: props.channelType,
      responses_websocket_enabled: props.enabled,
      ...props.defaults,
    },
  })
  return (
    <Form {...form}>
      <form
        onSubmit={form.handleSubmit((values) =>
          props.onSave(buildSettingJSON(values))
        )}
      >
        {props.bridgeSettings ? (
          <ResponsesViaChatSettings
            channelType={props.channelType}
            disabled={props.disabled}
          />
        ) : (
          <ResponsesWebSocketSetting
            channelType={props.channelType}
            disabled={props.disabled}
          />
        )}
        <button type='submit'>Save</button>
      </form>
    </Form>
  )
}

describe('Responses WebSocket channel setting', () => {
  describe.each(supportedChannelTypes)('channel type %s', (channelType) => {
    test.each([undefined, false, true])(
      'preserves setting %s through create, update and reload',
      (enabled) => {
        const channel = channelSchema.parse({
          id: 1,
          name: 'Test channel',
          key: '',
          type: channelType,
          status: 1,
          created_time: 0,
          test_time: 0,
          response_time: 0,
          balance_updated_time: 0,
          setting: JSON.stringify({ responses_websocket_enabled: enabled }),
        })
        const values = transformChannelToFormDefaults(channel)
        const payloads = [
          transformFormDataToCreatePayload(values).channel,
          transformFormDataToUpdatePayload(values, channel.id),
        ]
        for (const payload of payloads) {
          assert(typeof payload.setting === 'string')
          expect(JSON.parse(payload.setting).responses_websocket_enabled).toBe(
            enabled === true
          )
          expect(
            transformChannelToFormDefaults({
              ...channel,
              setting: payload.setting,
            }).responses_websocket_enabled
          ).toBe(enabled === true)
        }
      }
    )
  })

  test('new channels default to WebSocket disabled', () => {
    expect(
      JSON.parse(buildSettingJSON(CHANNEL_FORM_DEFAULT_VALUES))
        .responses_websocket_enabled
    ).toBe(false)
  })

  test.each(supportedChannelTypes)(
    'channel type %s exposes an accessible switch and saves on and off',
    async (channelType) => {
      const user = userEvent.setup()
      const saved: string[] = []
      render(
        <SettingsForm
          channelType={channelType}
          onSave={(value) => saved.push(value)}
        />
      )
      const toggle = screen.getByRole('switch', {
        name: 'Enable Responses WebSocket',
      })
      expect(toggle).not.toBeChecked()
      await user.click(toggle)
      await user.click(screen.getByRole('button', { name: 'Save' }))
      expect(JSON.parse(saved[0]).responses_websocket_enabled).toBe(true)
      toggle.focus()
      await user.keyboard(' ')
      await user.click(screen.getByRole('button', { name: 'Save' }))
      expect(JSON.parse(saved[1]).responses_websocket_enabled).toBe(false)
    }
  )

  test('unsupported channels hide the switch and do not save it as enabled', async () => {
    const user = userEvent.setup()
    const saved: string[] = []
    render(
      <SettingsForm
        channelType={14}
        enabled
        onSave={(value) => saved.push(value)}
      />
    )
    expect(screen.queryByRole('switch')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(JSON.parse(saved[0]).responses_websocket_enabled).toBe(false)
  })

  test('locked channel settings prevent toggling', async () => {
    const user = userEvent.setup()
    render(
      <SettingsForm channelType={1} disabled enabled onSave={() => undefined} />
    )
    const toggle = screen.getByRole('switch', {
      name: 'Enable Responses WebSocket',
    })
    expect(toggle).toHaveAttribute('aria-disabled', 'true')
    await user.click(toggle)
    expect(toggle).toBeChecked()
  })
})

describe('Responses via Chat settings', () => {
  const enabledSettings = {
    responses_via_chat: true,
    responses_chat_compatibility: 'legacy' as const,
    responses_local_replay: true,
    preserve_history_thinking: true,
  }

  test('unchanged channels omit all new opt-in settings', () => {
    const settings = JSON.parse(buildSettingJSON(CHANNEL_FORM_DEFAULT_VALUES))
    for (const field of Object.keys(enabledSettings)) {
      expect(settings).not.toHaveProperty(field)
    }
  })

  test.each([1, 3, 43])(
    'channel type %s preserves opt-ins through create, update and reload',
    (type) => {
      const channel = channelSchema.parse({
        id: 1,
        name: 'Chat bridge',
        key: '',
        type,
        status: 1,
        created_time: 0,
        test_time: 0,
        response_time: 0,
        balance_updated_time: 0,
        setting: JSON.stringify({
          ...enabledSettings,
          thinking_to_content: true,
          proxy: 'http://localhost:8080',
        }),
      })
      const values = transformChannelToFormDefaults(channel)
      for (const payload of [
        transformFormDataToCreatePayload(values).channel,
        transformFormDataToUpdatePayload(values, channel.id),
      ]) {
        assert(typeof payload.setting === 'string')
        expect(JSON.parse(payload.setting)).toMatchObject({
          ...enabledSettings,
          thinking_to_content: true,
          proxy: 'http://localhost:8080',
        })
        expect(
          transformChannelToFormDefaults({
            ...channel,
            setting: payload.setting,
          })
        ).toMatchObject(enabledSettings)
      }
    }
  )

  test('bridge gates dependent switches and disabling it clears dependent opt-ins only', async () => {
    const user = userEvent.setup()
    const saved: string[] = []
    render(
      <SettingsForm
        channelType={1}
        bridgeSettings
        onSave={(value) => saved.push(value)}
      />
    )
    const bridge = screen.getByRole('switch', {
      name: 'Enable Responses via Chat',
    })
    const legacy = screen.getByRole('switch', {
      name: 'Legacy Chat compatibility',
    })
    const replay = screen.getByRole('switch', {
      name: 'Enable local Responses replay',
    })
    const thinking = screen.getByRole('switch', {
      name: 'Preserve history thinking',
    })
    expect(bridge).not.toBeChecked()
    expect(legacy).toHaveAttribute('aria-disabled', 'true')
    expect(replay).toHaveAttribute('aria-disabled', 'true')
    await user.click(thinking)
    await user.click(bridge)
    expect(legacy).not.toHaveAttribute('aria-disabled', 'true')
    expect(replay).not.toHaveAttribute('aria-disabled', 'true')
    await user.click(legacy)
    await user.click(replay)
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(JSON.parse(saved[0])).toMatchObject(enabledSettings)
    bridge.focus()
    await user.keyboard(' ')
    expect(legacy).not.toBeChecked()
    expect(replay).not.toBeChecked()
    expect(thinking).toBeChecked()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(JSON.parse(saved[1])).toHaveProperty(
      'preserve_history_thinking',
      true
    )
    expect(JSON.parse(saved[1])).not.toHaveProperty('responses_via_chat')
    expect(JSON.parse(saved[1])).not.toHaveProperty('responses_local_replay')
    expect(JSON.parse(saved[1])).not.toHaveProperty(
      'responses_chat_compatibility'
    )
  })

  test('unsupported channels expose only the independent history switch', () => {
    render(
      <SettingsForm channelType={14} bridgeSettings onSave={() => undefined} />
    )
    expect(
      screen.queryByRole('switch', { name: 'Enable Responses via Chat' })
    ).not.toBeInTheDocument()
    expect(
      screen.getByRole('switch', { name: 'Preserve history thinking' })
    ).not.toBeChecked()
  })

  test.each([
    'pass_through_body_enabled',
    'responses_websocket_enabled',
  ] as const)(
    '%s prevents bridge activation without changing existing settings',
    async (conflict) => {
      const user = userEvent.setup()
      const saved: string[] = []
      render(
        <SettingsForm
          channelType={1}
          bridgeSettings
          defaults={{ [conflict]: true }}
          onSave={(value) => saved.push(value)}
        />
      )
      const bridge = screen.getByRole('switch', {
        name: 'Enable Responses via Chat',
      })
      expect(bridge).toHaveAttribute('aria-disabled', 'true')
      await user.click(bridge)
      await user.click(screen.getByRole('button', { name: 'Save' }))
      expect(JSON.parse(saved[0])).toHaveProperty(conflict, true)
      expect(JSON.parse(saved[0])).not.toHaveProperty('responses_via_chat')
    }
  )

  test('locked channels prevent changes to all bridge and history controls', async () => {
    const user = userEvent.setup()
    render(
      <SettingsForm
        channelType={1}
        bridgeSettings
        disabled
        defaults={enabledSettings}
        onSave={() => undefined}
      />
    )
    for (const toggle of screen.getAllByRole('switch')) {
      expect(toggle).toHaveAttribute('aria-disabled', 'true')
      await user.click(toggle)
      expect(toggle).toBeChecked()
    }
  })

  test.each([
    { type: 14, responses_via_chat: true },
    { responses_via_chat: true, responses_websocket_enabled: true },
    { responses_via_chat: true, pass_through_body_enabled: true },
    { responses_via_chat: false, responses_local_replay: true },
    { responses_via_chat: false, responses_chat_compatibility: 'legacy' },
  ])(
    'invalid bridge combination produces an actionable bridge field error: %j',
    (settings) => {
      const result = channelFormSchema.safeParse({
        ...CHANNEL_FORM_DEFAULT_VALUES,
        ...settings,
      })
      expect(result.success).toBe(false)
      if (!result.success) {
        expect(
          result.error.issues.some(
            (issue) => issue.path[0] === 'responses_via_chat'
          )
        ).toBe(true)
      }
    }
  )
})
