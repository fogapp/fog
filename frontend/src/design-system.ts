// Export barrel for the design-system sync (see .design-sync/ at the repo
// root). The app never imports this file; it exists so the converter can
// bundle every component under one namespace with its real name — `export *`
// would drop the default exports that most components use.

export { Spinner, Loading, LoadError, StaleNote } from "./components/Async";
export { default as AppFolderChoices } from "./components/AppFolderChoices";
export { default as AvatarMenu } from "./components/AvatarMenu";
export { default as Badge } from "./components/Badge";
export { default as Button } from "./components/Button";
export { default as CommandPalette } from "./components/CommandPalette";
export { default as ContextMenu } from "./components/ContextMenu";
export { default as CopyButton } from "./components/CopyButton";
export { default as DockerInstall } from "./components/DockerInstall";
export { default as Field, Select, TextArea, FilterInput } from "./components/Field";
export { default as FileIcon } from "./components/FileIcon";
export { default as GitAccess } from "./components/GitAccess";
export { default as FeedbackButton } from "./components/FeedbackButton";
export { default as InboxBell } from "./components/InboxBell";
export { default as Layout } from "./components/Layout";
export { default as Logo } from "./components/Logo";
export { default as Markdown } from "./components/MarkdownImpl";
export { default as MetricsRibbon } from "./components/MetricsRibbon";
export { default as Modal } from "./components/Modal";
export { default as MountPlan } from "./components/MountPlan";
export { default as PageHeader } from "./components/PageHeader";
export { default as PathBrowser } from "./components/PathBrowser";
export { Section, SectionHead, StatusLine } from "./components/Section";
export { default as SettingRow, SettingRows, RowList, RowItem } from "./components/SettingRow";
export { default as Segmented } from "./components/Segmented";
export { default as SetupPanel } from "./components/SetupPanel";
export { default as Sidebar } from "./components/Sidebar";
export { default as SpecEditor } from "./components/SpecEditor";
export { default as StateBadge, StateDot } from "./components/StateBadge";
export { default as Tabs } from "./components/Tabs";
export { default as Terminal } from "./components/Terminal";
export { default as Toggle } from "./components/Toggle";
export { default as PeerShareComposer } from "./components/files/PeerShareComposer";
