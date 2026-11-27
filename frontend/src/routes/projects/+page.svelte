<script lang="ts">
	import { onMount } from 'svelte';
	import ExternalLink from '@lucide/svelte/icons/external-link';
	import FolderGit from '@lucide/svelte/icons/folder-git';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Plus from '@lucide/svelte/icons/plus';
	import RefreshCw from '@lucide/svelte/icons/refresh-cw';
	import ShieldAlert from '@lucide/svelte/icons/shield-alert';
	import ShieldCheck from '@lucide/svelte/icons/shield-check';
	import ShieldX from '@lucide/svelte/icons/shield-x';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import Webhook from '@lucide/svelte/icons/webhook';
	import { getProjectAccess, getProjects, openExternal, postProject } from '#lib/api.ts';
	import { dashboard } from '#lib/dashboard.svelte.ts';
	import { showModalDialog } from '#lib/dialog.ts';
	import { t } from '#lib/i18n.svelte.ts';
	import type {
		ProjectAccess,
		ProjectInfo,
		ProjectPermission,
		ProjectsPayload
	} from '#lib/types.ts';
	import { cn, humanAge } from '#lib/utils.ts';
	import Button from '#lib/components/ui/button.svelte';
	import Input from '#lib/components/ui/input.svelte';
	import Masthead from '#lib/components/masthead.svelte';
	import SettingsPanel from '#lib/components/settings-panel.svelte';

	// The list comes from the config and renders immediately; the role check
	// costs two GitHub requests per repository, so it runs as a second request
	// and fills the badges in place. Waiting for both made the page take eight
	// seconds with nine projects behind a proxy.

	const ACCESS_DOCS =
		'https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/managing-repository-settings/managing-teams-and-people-with-access-to-your-repository';

	let payload = $state<ProjectsPayload | null>(null);
	let access = $state<Record<string, ProjectAccess>>({});
	let loading = $state(true);
	let checking = $state(false);
	let busy = $state('');
	let error = $state('');
	let accessError = $state('');
	let notice = $state('');

	let addDialog = $state<HTMLDialogElement | null>(null);
	let removeDialog = $state<HTMLDialogElement | null>(null);
	let addCleanup: (() => void) | null = null;
	let removeCleanup: (() => void) | null = null;

	let addName = $state('');
	let addError = $state('');
	let removeTarget = $state<ProjectInfo | null>(null);

	let repos = $derived(payload?.repos ?? []);
	let rows = $derived(repos.map((repo) => ({ repo, access: access[repo.name] })));
	let needAccess = $derived(rows.filter((row) => row.access && !row.access.canPush));
	let allGood = $derived(rows.length > 0 && rows.every((row) => row.access?.canPush));
	let pending = $derived(rows.filter((row) => !row.access));

	function message(err: unknown): string {
		return err instanceof Error ? err.message : String(err);
	}

	function toggleSettings(): void {
		if (dashboard.settingsOpen) dashboard.closeSettings();
		else dashboard.openSettings();
	}

	function pollAge(iso: string): string {
		if (!iso) return t('projects.neverPolled');
		const at = Date.parse(iso);
		if (Number.isNaN(at)) return t('projects.neverPolled');
		return t('projects.lastPoll', { age: humanAge((Date.now() - at) / 1000) });
	}

	function permissionLabel(permission: ProjectPermission): string {
		switch (permission) {
			case 'admin':
				return t('projects.perm.admin');
			case 'write':
				return t('projects.perm.write');
			case 'triage':
				return t('projects.perm.triage');
			case 'read':
				return t('projects.perm.read');
			case 'none':
				return t('projects.perm.none');
			default:
				return t('projects.perm.unknown');
		}
	}

	function permissionWhy(permission: ProjectPermission): string {
		switch (permission) {
			case 'read':
				return t('projects.permWhy.read');
			case 'triage':
				return t('projects.permWhy.triage');
			case 'none':
				return t('projects.permWhy.none');
			default:
				return t('projects.permWhy.unknown');
		}
	}

	function webhookLabel(repo: ProjectInfo): string {
		if (!payload?.webhook) return t('projects.webhook.off');
		if (repo.hookId > 0) return t('projects.webhook.managed');
		if (repo.smee) return t('projects.webhook.external');
		return '–';
	}

	async function load(): Promise<void> {
		loading = true;
		try {
			payload = (await getProjects()) as ProjectsPayload;
			error = '';
			// A reload (after a restart) re-checks the roles for exactly these
			// projects; stale entries for removed ones must not leak in.
			access = {};
		} catch (err) {
			error = t('projects.checkFailed', { msg: message(err) });
		} finally {
			loading = false;
		}
		if (payload) void checkAccess();
	}

	async function checkAccess(): Promise<void> {
		checking = true;
		accessError = '';
		try {
			const data = await getProjectAccess();
			const next: Record<string, ProjectAccess> = {};
			for (const item of data.repos as ProjectAccess[]) next[item.name] = item;
			access = next;
			accessError = data.error ?? '';
		} catch (err) {
			accessError = t('projects.checkFailed', { msg: message(err) });
		} finally {
			checking = false;
		}
	}

	// A write restarts the engine, so the listener answers with a connection
	// error for a moment. Retry instead of showing "daemon down" for a save the
	// user just made.
	async function reloadAfterRestart(): Promise<void> {
		busy = 'restart';
		for (let attempt = 0; attempt < 60; attempt++) {
			await new Promise((resolve) => setTimeout(resolve, 1000));
			try {
				payload = (await getProjects()) as ProjectsPayload;
				error = '';
				busy = '';
				void checkAccess();
				return;
			} catch {
				/* still restarting */
			}
		}
		busy = '';
		error = t('projects.checkFailed', { msg: 'timeout' });
	}

	function openAdd(): void {
		addName = '';
		addError = '';
		if (addDialog) addCleanup = showModalDialog(addDialog);
	}

	function openRemove(repo: ProjectInfo): void {
		removeTarget = repo;
		if (removeDialog) removeCleanup = showModalDialog(removeDialog);
	}

	async function submitAdd(event: SubmitEvent): Promise<void> {
		event.preventDefault();
		const name = addName.trim();
		if (!name) return;
		busy = 'add';
		addError = '';
		try {
			const result = await postProject('add', name);
			addDialog?.close();
			notice = t('projects.added', { repo: result.repo ?? name }) + ' · ' + t('projects.saved');
			// reloadAfterRestart owns `busy` from here: the request listener is
			// about to go away and clears the state when the daemon answers.
			void reloadAfterRestart();
		} catch (err) {
			addError = t('projects.failed', { msg: message(err) });
			busy = '';
		}
	}

	async function submitRemove(): Promise<void> {
		const target = removeTarget;
		if (!target) return;
		busy = 'remove';
		try {
			await postProject('remove', target.name);
			removeDialog?.close();
			notice = t('projects.removed', { repo: target.name }) + ' · ' + t('projects.saved');
			void reloadAfterRestart();
		} catch (err) {
			error = t('projects.failed', { msg: message(err) });
			removeDialog?.close();
			busy = '';
		}
	}

	onMount(() => {
		const stopPolling = dashboard.start();
		void load();
		return () => {
			stopPolling();
		};
	});
</script>

<div class="flex min-h-screen flex-col">
	<Masthead onOpenSettings={toggleSettings} stats={false} />

	<main class="page-shell w-full flex-1 px-4 py-6 sm:px-6" data-testid="projects-page">
		<div class="mb-5 flex flex-wrap items-start gap-x-4 gap-y-3">
			<div class="min-w-0 flex-1">
				<h2 class="font-sans text-xl font-semibold">{t('projects.title')}</h2>
				<p class="mt-1 text-sm text-muted-foreground">{t('projects.subtitle')}</p>
			</div>
			<div class="flex flex-none flex-wrap items-center gap-2">
				<Button
					variant="outline"
					size="sm"
					disabled={loading || busy !== ''}
					onclick={() => void load()}
					data-testid="projects-refresh"
				>
					<RefreshCw
						class={cn('size-4', (loading || checking) && 'animate-spin')}
						aria-hidden="true"
					/>
					{t('projects.refresh')}
				</Button>
				<Button
					variant="default"
					size="sm"
					disabled={busy !== ''}
					onclick={openAdd}
					data-testid="projects-add"
				>
					<Plus class="size-4" aria-hidden="true" />
					{t('projects.add')}
				</Button>
			</div>
		</div>

		<div class="mb-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
			{#if payload?.bot}
				<span>
					{t('projects.bot')}:
					<span class="font-mono text-foreground">@{payload.bot}</span>
				</span>
			{/if}
			<span>{t('projects.count', { n: repos.length })}</span>
			{#if checking}
				<span class="flex items-center gap-1" data-testid="projects-checking">
					<LoaderCircle class="size-3 animate-spin" aria-hidden="true" />
					{t('projects.checkingShort')}
				</span>
			{/if}
			<span class="font-mono break-all">{payload?.config ?? ''}</span>
		</div>

		<p
			class={cn('text-sm', notice ? 'mb-3 text-phase-done' : 'sr-only')}
			role="status"
			aria-live="polite"
			data-testid="projects-notice"
		>
			{notice}
		</p>

		{#if error}
			<p
				class="mb-3 flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
				role="alert"
				data-testid="projects-error"
			>
				<ShieldX class="mt-0.5 size-4 flex-none" aria-hidden="true" />
				<span>{error}</span>
			</p>
		{/if}

		{#if loading && !payload}
			<p class="py-10 text-center text-sm text-muted-foreground">{t('projects.loading')}</p>
		{:else if repos.length === 0}
			<div class="card flex flex-col items-center gap-2 px-6 py-10 text-center">
				<FolderGit class="size-8 text-muted-foreground" aria-hidden="true" />
				<p class="text-sm text-muted-foreground">{t('projects.empty')}</p>
			</div>
		{:else}
			{#if needAccess.length > 0}
				<section
					class="mb-4 rounded-lg border border-phase-work/40 bg-phase-work/5 p-4"
					aria-labelledby="projects-guide-title"
					data-testid="projects-guide"
				>
					<h3
						id="projects-guide-title"
						class="flex items-center gap-2 font-sans text-sm font-semibold"
					>
						<ShieldAlert class="size-4 text-phase-work" aria-hidden="true" />
						{t('projects.guide.title')}
					</h3>
					<p class="mt-1 text-sm text-muted-foreground">
						{t('projects.guide.body', { bot: payload?.bot ?? 'bot' })}
					</p>
					<ul class="mt-2 flex flex-wrap items-center gap-2">
						{#each needAccess as row (row.repo.name)}
							<li>
								<button
									type="button"
									class="chip h-8 gap-1.5 text-xs"
									onclick={() => openExternal(row.repo.accessUrl)}
								>
									<ExternalLink class="size-3.5" aria-hidden="true" />
									{t('projects.guide.open')} · {row.repo.name}
								</button>
							</li>
						{/each}
						<li>
							<button
								type="button"
								class="text-xs text-muted-foreground underline underline-offset-2 hover:text-foreground"
								onclick={() => openExternal(ACCESS_DOCS)}
							>
								{t('projects.guide.docs')}
							</button>
						</li>
					</ul>
				</section>
			{:else if allGood}
				<p
					class="mb-4 flex items-center gap-2 rounded-lg border border-phase-done/30 bg-phase-done/5 px-3 py-2 text-sm"
					data-testid="projects-guide-ok"
				>
					<ShieldCheck class="size-4 text-phase-done" aria-hidden="true" />
					{t('projects.guide.allGood')}
				</p>
			{/if}

			{#if accessError}
				<p
					class="mb-3 flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive"
					role="alert"
					data-testid="projects-access-error"
				>
					<ShieldX class="mt-0.5 size-4 flex-none" aria-hidden="true" />
					<span>{accessError}</span>
				</p>
			{/if}

			<ul class="flex flex-col gap-2" data-testid="projects-list">
				{#each rows as row (row.repo.name)}
					{@const repo = row.repo}
					{@const checked = row.access}
					<li class="card px-4 py-3" data-testid="project-row" data-repo={repo.name}>
						<div class="flex flex-wrap items-center gap-x-3 gap-y-2">
							<h3 class="flex min-w-0 items-center gap-2 font-mono text-sm font-semibold">
								<FolderGit class="size-4 flex-none text-muted-foreground" aria-hidden="true" />
								<span class="break-words">{repo.name}</span>
							</h3>
							{#if checked}
								<span
									class={cn(
										'inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-semibold',
										checked.canPush
											? 'border-phase-done/40 bg-phase-done/10 text-phase-done'
											: 'border-phase-blocked/40 bg-phase-blocked/10 text-phase-blocked'
									)}
									data-testid="project-permission"
									title={checked.canPush ? undefined : permissionWhy(checked.permission)}
								>
									{#if checked.canPush}
										<ShieldCheck class="size-3.5" aria-hidden="true" />
									{:else}
										<ShieldAlert class="size-3.5" aria-hidden="true" />
									{/if}
									{t('projects.col.permission')} · {permissionLabel(checked.permission)}
								</span>
							{:else}
								<span
									class="inline-flex items-center gap-1.5 rounded-full border border-border bg-muted/40 px-2.5 py-0.5 text-xs font-medium text-muted-foreground"
									data-testid="project-checking"
								>
									<LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
									{t('projects.checking')}
								</span>
							{/if}
							<Button
								variant="destructive"
								size="sm"
								class="ml-auto"
								aria-label={t('projects.removeAria', { repo: repo.name })}
								disabled={busy !== ''}
								onclick={() => openRemove(repo)}
								data-testid="project-remove"
							>
								<Trash2 class="size-3.5" aria-hidden="true" />
								{t('projects.remove')}
							</Button>
						</div>

						<dl
							class="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-muted-foreground sm:grid-cols-4"
						>
							<div>
								<dt class="text-2xs tracking-[0.1em] uppercase">{t('projects.col.base')}</dt>
								<dd class="font-mono text-foreground">{repo.base || '–'}</dd>
							</div>
							<div>
								<dt class="text-2xs tracking-[0.1em] uppercase">{t('projects.col.agent')}</dt>
								<dd class="text-foreground">
									{repo.agent}{repo.model ? ' · ' + repo.model : ''}
								</dd>
							</div>
							<div>
								<dt class="text-2xs tracking-[0.1em] uppercase">{t('projects.col.webhook')}</dt>
								<dd class="flex items-center gap-1.5 text-foreground">
									<Webhook class="size-3.5 flex-none" aria-hidden="true" />
									<span>{webhookLabel(repo)}</span>
									{#if repo.hookId > 0}
										<span class="whitespace-nowrap text-muted-foreground"
											>{t('projects.webhook.hint', { id: repo.hookId })}</span
										>
									{/if}
								</dd>
							</div>
							<div>
								<dt class="text-2xs tracking-[0.1em] uppercase">{t('projects.col.lastPoll')}</dt>
								<dd class="text-foreground">{pollAge(repo.lastPoll)}</dd>
							</div>
						</dl>

						{#if checked && !checked.canPush}
							<p class="mt-2 text-xs text-phase-blocked" data-testid="project-permission-why">
								{permissionWhy(checked.permission)}
								{#if checked.error}
									<span class="text-muted-foreground"> — {checked.error}</span>
								{/if}
								<button
									type="button"
									class="ml-1 underline underline-offset-2 hover:text-foreground"
									onclick={() => openExternal(repo.accessUrl)}
								>
									{t('projects.guide.open')}
								</button>
							</p>
						{/if}
					</li>
				{/each}
			</ul>
			{#if pending.length > 0 && !checking && accessError}
				<p class="mt-3 text-xs text-muted-foreground">{t('projects.accessUnavailable')}</p>
			{/if}
		{/if}
	</main>
</div>

<dialog
	bind:this={addDialog}
	class="page-dialog"
	closedby="any"
	aria-labelledby="project-add-title"
	data-testid="project-add-dialog"
	onclose={() => {
		addCleanup?.();
		addCleanup = null;
	}}
>
	<form class="flex flex-col gap-3 p-5" onsubmit={submitAdd}>
		<div class="flex items-start gap-3">
			<div class="min-w-0">
				<h2 id="project-add-title" class="font-sans text-lg font-semibold">
					{t('projects.addTitle')}
				</h2>
				<p class="mt-1 text-xs text-muted-foreground">{t('projects.addHint')}</p>
			</div>
		</div>
		<label class="flex flex-col gap-1.5 text-sm font-medium">
			{t('projects.addLabel')}
			<Input
				bind:value={addName}
				placeholder={t('projects.addPlaceholder')}
				autocomplete="off"
				spellcheck="false"
				required
				autofocus
				data-testid="project-add-input"
			/>
		</label>
		{#if addError}
			<p class="text-sm text-destructive" role="alert" data-testid="project-add-error">
				{addError}
			</p>
		{/if}
		<div class="flex justify-end gap-2">
			<Button type="button" variant="ghost" onclick={() => addDialog?.close()}>
				{t('projects.cancel')}
			</Button>
			<Button type="submit" variant="default" disabled={busy === 'add'}>
				<Plus class="size-4" aria-hidden="true" />
				{busy === 'add' ? t('projects.adding') : t('projects.addSubmit')}
			</Button>
		</div>
	</form>
</dialog>

<dialog
	bind:this={removeDialog}
	class="page-dialog"
	closedby="any"
	aria-labelledby="project-remove-title"
	aria-describedby="project-remove-body"
	data-testid="project-remove-dialog"
	onclose={() => {
		removeCleanup?.();
		removeCleanup = null;
		removeTarget = null;
	}}
>
	<div class="flex flex-col gap-3 p-5">
		<h2 id="project-remove-title" class="font-sans text-lg font-semibold">
			{t('projects.removeTitle')}
		</h2>
		<p id="project-remove-body" class="text-sm text-muted-foreground">
			{t('projects.removeBody', { repo: removeTarget?.name ?? '' })}
		</p>
		<div class="flex justify-end gap-2">
			<!-- autofocus on the safe action: a stray Enter must not remove the project -->
			<Button type="button" variant="ghost" autofocus onclick={() => removeDialog?.close()}>
				{t('projects.cancel')}
			</Button>
			<Button
				type="button"
				variant="destructive"
				disabled={busy === 'remove'}
				onclick={() => void submitRemove()}
				data-testid="project-remove-confirm"
			>
				<Trash2 class="size-4" aria-hidden="true" />
				{busy === 'remove' ? t('projects.removing') : t('projects.removeConfirm')}
			</Button>
		</div>
	</div>
</dialog>

{#if dashboard.settingsOpen}
	<SettingsPanel />
{/if}

<style>
	.page-dialog {
		inline-size: min(30rem, 92vw);
		max-inline-size: none;
		margin: auto;
		padding: 0;
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		background: var(--card);
		color: var(--card-foreground);
		box-shadow: var(--shadow-float);
	}

	.page-dialog::backdrop {
		background: oklch(0 0 0 / 0.5);
		backdrop-filter: blur(2px);
	}
</style>
