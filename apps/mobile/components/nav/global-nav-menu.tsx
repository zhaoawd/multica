/**
 * GlobalNavMenu — bottom-right popover anchored above the More tab. Three
 * sections: user identity card → workspace switcher → real feature entries.
 *
 * Why a popover and not a tab: the iOS HIG treats tab-bar items as
 * destinations, not action triggers, so "More" was an anti-pattern. Linear /
 * Things 3 / Reminders all use a header-anchored global nav button instead.
 *
 * Why custom Modal instead of @gorhom/bottom-sheet: gorhom v5 only supports
 * Reanimated v3 and the mobile app is on Reanimated v4. Same Modal+Pressable
 * pattern as status-picker-sheet.tsx etc. — keeps the dependency surface
 * untouched.
 *
 * Composition mirrors web's sidebar dropdown (packages/views/layout/
 * app-sidebar.tsx:496-511): user info row (avatar + name + email) sits above
 * the workspace list. On mobile the row is a tappable card that pushes into
 * the existing settings page, since there isn't enough screen real estate to
 * inline account / workspaces / sign-out the way web does.
 */
import { useMemo, useState } from "react";
import {
  ActivityIndicator,
  Image,
  Modal,
  Pressable,
  ScrollView,
  View,
} from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";
import { Ionicons } from "@expo/vector-icons";
import { router, usePathname } from "expo-router";
import { useQuery } from "@tanstack/react-query";
import type { User, Workspace } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import { workspaceListOptions } from "@/data/queries/workspaces";
import { useAuthStore } from "@/data/auth-store";
import { useWorkspaceStore } from "@/data/workspace-store";
import { cn } from "@/lib/utils";

interface NavItem {
  label: string;
  icon: keyof typeof Ionicons.glyphMap;
  /** Path under /:slug/ — final href is `/${slug}${path}`. */
  path: string;
}

// Inbox / My Issues / Chat live on the bottom tab bar; Settings is reached
// via the user card at the top of this popover. Only entries that are NOT
// covered by either of those surfaces belong here.
const NAV_ITEMS: NavItem[] = [
  { label: "Projects", icon: "cube-outline", path: "/more/projects" },
];

const ICON_COLOR = "#3f3f46";
const ICON_MUTED = "#71717a";

interface Props {
  visible: boolean;
  onClose: () => void;
}

export function GlobalNavMenu({ visible, onClose }: Props) {
  const insets = useSafeAreaInsets();
  const slug = useWorkspaceStore((s) => s.currentWorkspaceSlug);
  const user = useAuthStore((s) => s.user);
  const pathname = usePathname();
  const [showWorkspaces, setShowWorkspaces] = useState(false);

  const currentWorkspace = useCurrentWorkspace(slug);

  const isActive = (path: string) => {
    if (!slug) return false;
    const target = `/${slug}${path}`;
    // Match exact, or a deeper child route. Append `/` to the prefix so a
    // sibling like /:slug/inbox-archive doesn't match /:slug/inbox.
    if (pathname === target) return true;
    return pathname.startsWith(target + "/");
  };

  const onNav = (path: string) => {
    if (!slug) return;
    onClose();
    setShowWorkspaces(false);
    router.push(`/${slug}${path}`);
  };

  const onOpenSettings = () => {
    if (!slug) return;
    onClose();
    setShowWorkspaces(false);
    router.push(`/${slug}/more/settings`);
  };

  return (
    <Modal
      visible={visible}
      transparent
      animationType="fade"
      onRequestClose={onClose}
    >
      <Pressable
        className="flex-1 bg-black/30"
        onPress={() => {
          setShowWorkspaces(false);
          onClose();
        }}
      >
        <View
          // Anchor above the bottom tab bar (49pt iOS default + bottom
          // safe-area inset for the home indicator) with a hair of
          // breathing room. Menu rises from the More tab on the right.
          style={{ paddingBottom: insets.bottom + 49 + 8, paddingRight: 12 }}
          className="flex-1 items-end justify-end"
        >
          <Pressable onPress={() => {}}>
            <View
              className="w-72 bg-popover rounded-2xl overflow-hidden"
              // Subtle elevation so it visually lifts off the page.
              style={{
                shadowColor: "#000",
                shadowOpacity: 0.18,
                shadowRadius: 16,
                shadowOffset: { width: 0, height: 8 },
                elevation: 8,
              }}
            >
              {/* User identity card — tap pushes into settings, where
                  account info, workspace list, and sign out already live. */}
              <UserCard user={user} onPress={onOpenSettings} />

              {/* Workspace switcher header */}
              <Pressable
                onPress={() => setShowWorkspaces((v) => !v)}
                className="flex-row items-center px-4 py-3 active:bg-secondary border-b border-border"
              >
                <View className="size-7 rounded-md bg-secondary items-center justify-center mr-3">
                  <Ionicons name="business" size={14} color={ICON_COLOR} />
                </View>
                <Text
                  className="flex-1 text-sm font-medium text-foreground"
                  numberOfLines={1}
                >
                  {currentWorkspace?.name ?? "Workspace"}
                </Text>
                <Ionicons
                  name={showWorkspaces ? "chevron-up" : "chevron-down"}
                  size={14}
                  color={ICON_MUTED}
                />
              </Pressable>

              {showWorkspaces ? (
                <WorkspaceList
                  activeSlug={slug}
                  onPick={(ws) => {
                    setShowWorkspaces(false);
                    onClose();
                    router.replace(`/${ws.slug}/inbox`);
                  }}
                />
              ) : (
                <View className="py-1">
                  {NAV_ITEMS.map((item) => {
                    const active = isActive(item.path);
                    return (
                      <Pressable
                        key={item.path}
                        onPress={() => onNav(item.path)}
                        className={cn(
                          "flex-row items-center px-3 py-2.5 mx-1 rounded-lg active:bg-secondary",
                          active && "bg-secondary",
                        )}
                      >
                        <Ionicons
                          name={item.icon}
                          size={18}
                          color={ICON_COLOR}
                        />
                        <Text className="ml-3 flex-1 text-sm text-foreground">
                          {item.label}
                        </Text>
                      </Pressable>
                    );
                  })}
                </View>
              )}
            </View>
          </Pressable>
        </View>
      </Pressable>
    </Modal>
  );
}

function WorkspaceList({
  activeSlug,
  onPick,
}: {
  activeSlug: string | null;
  onPick: (ws: Workspace) => void;
}) {
  const { data, isLoading, error } = useQuery(workspaceListOptions());

  if (isLoading) {
    return (
      <View className="py-6 items-center">
        <ActivityIndicator />
      </View>
    );
  }

  if (error) {
    return (
      <View className="px-4 py-4">
        <Text className="text-sm text-destructive">
          Failed to load workspaces
        </Text>
      </View>
    );
  }

  return (
    <ScrollView className="max-h-72">
      {data?.map((ws) => {
        const active = ws.slug === activeSlug;
        return (
          <Pressable
            key={ws.id}
            onPress={() => {
              if (active) return;
              onPick(ws);
            }}
            disabled={active}
            className="flex-row items-center px-4 py-3 active:bg-secondary"
          >
            <View className="flex-1">
              <Text
                className="text-sm font-medium text-foreground"
                numberOfLines={1}
              >
                {ws.name}
              </Text>
              <Text className="text-xs text-muted-foreground mt-0.5">
                /{ws.slug}
              </Text>
            </View>
            {active ? (
              <Ionicons name="checkmark" size={16} color={ICON_MUTED} />
            ) : null}
          </Pressable>
        );
      })}
    </ScrollView>
  );
}

function useCurrentWorkspace(slug: string | null): Workspace | undefined {
  const { data } = useQuery(workspaceListOptions());
  return useMemo(
    () => (slug ? data?.find((w) => w.slug === slug) : undefined),
    [data, slug],
  );
}

function UserCard({
  user,
  onPress,
}: {
  user: User | null;
  onPress: () => void;
}) {
  const initial = (user?.name ?? user?.email ?? "U").charAt(0).toUpperCase();
  return (
    <Pressable
      onPress={onPress}
      className="flex-row items-center gap-3 px-4 py-3.5 active:bg-secondary border-b border-border"
    >
      {user?.avatar_url ? (
        <Image
          source={{ uri: user.avatar_url }}
          className="size-10 rounded-full bg-muted"
        />
      ) : (
        <View className="size-10 rounded-full bg-muted items-center justify-center">
          <Text className="text-sm font-medium text-muted-foreground">
            {initial}
          </Text>
        </View>
      )}
      <View className="flex-1 min-w-0">
        <Text
          className="text-sm font-medium text-foreground"
          numberOfLines={1}
        >
          {user?.name ?? "—"}
        </Text>
        {user?.email ? (
          <Text className="text-xs text-muted-foreground mt-0.5" numberOfLines={1}>
            {user.email}
          </Text>
        ) : null}
      </View>
      <Ionicons name="chevron-forward" size={16} color={ICON_MUTED} />
    </Pressable>
  );
}
