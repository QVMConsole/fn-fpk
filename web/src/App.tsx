import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import Banner from '@douyinfe/semi-ui/lib/es/banner';
import Button from '@douyinfe/semi-ui/lib/es/button';
import Card from '@douyinfe/semi-ui/lib/es/card';
import Checkbox from '@douyinfe/semi-ui/lib/es/checkbox';
import Descriptions from '@douyinfe/semi-ui/lib/es/descriptions';
import Input from '@douyinfe/semi-ui/lib/es/input';
import InputNumber from '@douyinfe/semi-ui/lib/es/inputNumber';
import Layout from '@douyinfe/semi-ui/lib/es/layout';
import Modal from '@douyinfe/semi-ui/lib/es/modal';
import Progress from '@douyinfe/semi-ui/lib/es/progress';
import SideSheet from '@douyinfe/semi-ui/lib/es/sideSheet';
import Select from '@douyinfe/semi-ui/lib/es/select';
import Space from '@douyinfe/semi-ui/lib/es/space';
import Spin from '@douyinfe/semi-ui/lib/es/spin';
import Table from '@douyinfe/semi-ui/lib/es/table';
import Tabs from '@douyinfe/semi-ui/lib/es/tabs';
import Tag from '@douyinfe/semi-ui/lib/es/tag';
import TextArea from '@douyinfe/semi-ui/lib/es/input/textarea';
import Toast from '@douyinfe/semi-ui/lib/es/toast';
import Tooltip from '@douyinfe/semi-ui/lib/es/tooltip';
import Typography from '@douyinfe/semi-ui/lib/es/typography';
import IconDelete from '@douyinfe/semi-icons/lib/es/icons/IconDelete';
import IconExternalOpen from '@douyinfe/semi-icons/lib/es/icons/IconExternalOpen';
import IconKey from '@douyinfe/semi-icons/lib/es/icons/IconKey';
import IconPlay from '@douyinfe/semi-icons/lib/es/icons/IconPlay';
import IconRefresh from '@douyinfe/semi-icons/lib/es/icons/IconRefresh';
import IconSetting from '@douyinfe/semi-icons/lib/es/icons/IconSetting';
import IconStop from '@douyinfe/semi-icons/lib/es/icons/IconStop';
import { ApiError, apiPath, loadSession, request } from './api';
import type { Channel, Job, Session, Status, StorageOption, User } from './types';

const { Header, Content } = Layout;
const { Title, Text, Paragraph } = Typography;
const { TabPane } = Tabs;

type JobAction = 'install' | 'update' | 'switch' | 'repair' | 'uninstall';

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : '发生未知错误';
}

function stateTag(active: boolean, positive = '正常', negative = '异常') {
  return <Tag color={active ? 'green' : 'red'}>{active ? positive : negative}</Tag>;
}

function channelName(id: string, channels: Channel[]): string {
  return channels.find((item) => item.id === id)?.name || (id ? '未知渠道' : '未记录');
}

function formatStorageSize(bytes = 0): string {
  if (bytes <= 0) return '未知';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value >= 10 || unit === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[unit]}`;
}

function storageOptionLabel(option: StorageOption): string {
  const name = option.default ? '根目录（默认）' : option.path;
  return `${name} · 可用 ${formatStorageSize(option.availableBytes)} / ${formatStorageSize(option.totalBytes)}`;
}

export default function App() {
  const [session, setSession] = useState<Session>();
  const [status, setStatus] = useState<Status>();
  const [channels, setChannels] = useState<Channel[]>([]);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [users, setUsers] = useState<User[]>([]);
  const [loading, setLoading] = useState(true);
  const [selectedChannel, setSelectedChannel] = useState<'open' | 'sponsor'>('open');
  const [installPort, setInstallPort] = useState<number>(8080);
  const [storageOptions, setStorageOptions] = useState<StorageOption[]>([]);
  const [selectedStorageDir, setSelectedStorageDir] = useState('');
  const [storageOptionsLoading, setStorageOptionsLoading] = useState(false);
  const [riskConfirmed, setRiskConfirmed] = useState(false);
  const [dependencyConfirmed, setDependencyConfirmed] = useState(false);
  const [activeJob, setActiveJob] = useState<Job>();
  const [jobLog, setJobLog] = useState('');
  const [jobPanelVisible, setJobPanelVisible] = useState(false);
  const jobLogContainerRef = useRef<HTMLDivElement>(null);
  const [serviceLog, setServiceLog] = useState('');
  const [serviceLogLoading, setServiceLogLoading] = useState(false);

  const [uninstallVisible, setUninstallVisible] = useState(false);
  const [purge, setPurge] = useState(false);
  const [uninstallConfirmation, setUninstallConfirmation] = useState('');
  const [resetVisible, setResetVisible] = useState(false);
  const [resetConfirmation, setResetConfirmation] = useState('');
  const [totpUser, setTotpUser] = useState<User>();
  const [totpConfirmation, setTotpConfirmation] = useState('');
  const [passwordUser, setPasswordUser] = useState<User>();
  const [password, setPassword] = useState('');
  const [passwordAgain, setPasswordAgain] = useState('');
  const [passwordConfirmation, setPasswordConfirmation] = useState('');
  const [portVisible, setPortVisible] = useState(false);
  const [newPort, setNewPort] = useState(8080);
  const [portConfirmation, setPortConfirmation] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const loadStatus = useCallback(async () => {
    const data = await request<Status>('/api/v1/status');
    setStatus(data);
    if (data.port) {
      setInstallPort(data.port);
      setNewPort(data.port);
    }
    if (data.activeJob) {
      setActiveJob(data.activeJob);
    }
  }, []);

  const loadJobs = useCallback(async () => {
    setJobs(await request<Job[]>('/api/v1/jobs'));
  }, []);

  const loadStorageOptions = useCallback(async () => {
    try {
      setStorageOptionsLoading(true);
      const options = await request<StorageOption[]>('/api/v1/storage-options');
      setStorageOptions(options);
      setSelectedStorageDir((current) => options.some((option) => option.path === current) ? current : (options[0]?.path || ''));
    } catch (error) {
      setStorageOptions([]);
      setSelectedStorageDir('');
      Toast.error(errorMessage(error));
    } finally {
      setStorageOptionsLoading(false);
    }
  }, []);

  const loadUsers = useCallback(async () => {
    if (!status?.databasePresent) return;
    try {
      setUsers(await request<User[]>('/api/v1/users'));
    } catch (error) {
      Toast.error(errorMessage(error));
    }
  }, [status?.databasePresent]);

  useEffect(() => {
    void (async () => {
      try {
        const currentSession = await loadSession();
        setSession(currentSession);
        const [statusData, channelData, jobData] = await Promise.all([
          request<Status>('/api/v1/status'),
          request<Channel[]>('/api/v1/channels'),
          request<Job[]>('/api/v1/jobs'),
        ]);
        setStatus(statusData);
        setChannels(channelData);
        setJobs(jobData);
        setInstallPort(statusData.port || 8080);
        setNewPort(statusData.port || 8080);
        setSelectedChannel(statusData.channel === 'sponsor' ? 'sponsor' : 'open');
        if (statusData.activeJob) setActiveJob(statusData.activeJob);
      } catch (error) {
        Toast.error(errorMessage(error));
      } finally {
        setLoading(false);
      }
    })();
  }, []);

  useEffect(() => {
    if (!status?.databasePresent) return;
    void loadUsers();
  }, [loadUsers, status?.databasePresent]);

  useEffect(() => {
    if (!status || status.installed) return;
    void loadStorageOptions();
  }, [loadStorageOptions, status?.installed]);

  useEffect(() => {
    if (!activeJob || !['queued', 'running'].includes(activeJob.status)) return;
    setJobPanelVisible(true);
    const source = new EventSource(apiPath(`/api/v1/jobs/${activeJob.id}/events`));
    source.addEventListener('log', (event) => {
      try {
        setJobLog((current) => current + (JSON.parse((event as MessageEvent).data) as string));
      } catch {
        setJobLog((current) => current + (event as MessageEvent).data);
      }
    });
    const timer = window.setInterval(async () => {
      try {
        const current = await request<Job>(`/api/v1/jobs/${activeJob.id}`);
        setActiveJob(current);
        if (!['queued', 'running'].includes(current.status)) {
          window.clearInterval(timer);
          source.close();
          await Promise.all([loadStatus(), loadJobs()]);
          if (current.status === 'succeeded') Toast.success('任务执行完成');
          else Toast.error(current.error || '任务执行失败');
        }
      } catch {
        // 网络短暂中断时保留任务面板，等待下一轮刷新。
      }
    }, 1500);
    return () => {
      window.clearInterval(timer);
      source.close();
    };
  }, [activeJob?.id, activeJob?.status, loadJobs, loadStatus]);

  useLayoutEffect(() => {
    if (!jobPanelVisible) return;
    const textarea = jobLogContainerRef.current?.querySelector<HTMLTextAreaElement>('textarea');
    if (!textarea) return;
    const scrollToBottom = () => {
      textarea.scrollTop = textarea.scrollHeight;
    };
    scrollToBottom();
    const frame = window.requestAnimationFrame(scrollToBottom);
    return () => window.cancelAnimationFrame(frame);
  }, [jobLog, jobPanelVisible]);

  const startJob = async (action: JobAction, options: { channel?: string; port?: number; storageDir?: string; purge?: boolean } = {}) => {
    try {
      setSubmitting(true);
      setJobLog('');
      const job = await request<Job>('/api/v1/jobs', {
        method: 'POST',
        body: JSON.stringify({ action, ...options }),
      });
      setActiveJob(job);
      setJobPanelVisible(true);
      setUninstallVisible(false);
    } catch (error) {
      Toast.error(errorMessage(error));
    } finally {
      setSubmitting(false);
    }
  };

  const serviceAction = async (action: 'start' | 'stop' | 'restart') => {
    try {
      setSubmitting(true);
      await request(`/api/v1/service/${action}`, { method: 'POST', body: '{}' });
      Toast.success(action === 'start' ? '服务已启动' : action === 'stop' ? '服务已停止' : '服务已重启');
      await loadStatus();
    } catch (error) {
      Toast.error(errorMessage(error));
    } finally {
      setSubmitting(false);
    }
  };

  const loadServiceLog = async () => {
    try {
      setServiceLogLoading(true);
      const result = await request<{ content: string }>('/api/v1/logs?lines=500');
      setServiceLog(result.content);
    } catch (error) {
      Toast.error(errorMessage(error));
    } finally {
      setServiceLogLoading(false);
    }
  };

  const changePassword = async (allowUnavailable = false) => {
    if (!passwordUser) return;
    try {
      setSubmitting(true);
      await request(`/api/v1/users/${passwordUser.id}/password`, {
        method: 'POST',
        body: JSON.stringify({
          password,
          confirmPassword: passwordAgain,
          confirmation: passwordConfirmation,
          allowUnavailable,
        }),
      });
      Toast.success('管理员密码已修改，邮箱与 TOTP 绑定保持不变');
      setPasswordUser(undefined);
      setPassword('');
      setPasswordAgain('');
      setPasswordConfirmation('');
    } catch (error) {
      if (error instanceof ApiError && error.code === 'password_check_unavailable' && !allowUnavailable) {
        Modal.confirm({
          title: '泄露检测暂时不可用',
          content: '在线泄露密码检测当前不可用。确认仍要使用这个密码吗？',
          okType: 'danger',
          onOk: () => changePassword(true),
        });
      } else {
        Toast.error(errorMessage(error));
      }
    } finally {
      setSubmitting(false);
    }
  };

  const openQVMConsole = () => {
    if (!status) return;
    window.open(`${window.location.protocol}//${window.location.hostname}:${status.port}`, '_blank', 'noopener,noreferrer');
  };

  const userColumns = useMemo(() => [
    { title: 'ID', dataIndex: 'id', width: 70 },
    { title: '用户名', dataIndex: 'username' },
    { title: '角色', dataIndex: 'role', render: (value: string) => <Tag color={value === 'admin' ? 'amber' : 'blue'}>{value}</Tag> },
    { title: '状态', dataIndex: 'status' },
    { title: 'TOTP', dataIndex: 'totpEnabled', render: (value: boolean) => stateTag(value, '已绑定', '未绑定') },
    { title: '邮箱', dataIndex: 'email', render: (value: string) => value || '-' },
    {
      title: '操作',
      width: 100,
      render: (_: unknown, user: User) => (
        <Space spacing={4}>
          <Tooltip content="修改管理员密码">
            <Button
              className="qvm-act-ic"
              theme="borderless"
              icon={<IconKey />}
              disabled={user.role !== 'admin'}
              onClick={() => setPasswordUser(user)}
            />
          </Tooltip>
          <Tooltip content="清除 TOTP">
            <Button
              className="qvm-act-ic"
              theme="borderless"
              type="danger"
              icon={<IconDelete />}
              disabled={!user.totpEnabled}
              onClick={() => setTotpUser(user)}
            />
          </Tooltip>
        </Space>
      ),
    },
  ], []);

  if (loading) {
    return <div className="loading-page"><Spin size="large" tip="正在连接管理器…" /></div>;
  }

  if (!session || !status) {
    return <div className="loading-page"><Banner type="danger" title="管理器初始化失败" description="请确认应用已启动并从飞牛桌面入口打开。" /></div>;
  }

  const busy = Boolean(activeJob && ['queued', 'running'].includes(activeJob.status));
  const selectedIsCurrent = selectedChannel === status.channel;
  const updateChannel = channels.some((channel) => channel.id === status.channel) ? status.channel : selectedChannel;
  const selectedStorage = storageOptions.find((option) => option.path === selectedStorageDir);

  return (
    <Layout className="app-shell">
      <Header className="app-header">
        <div>
          <div className="eyebrow">FNOS PACKAGE MANAGER</div>
          <Title heading={3} className="app-title">QVMConsole 管理器</Title>
        </div>
        <Space>
          <Tag color="blue">v{status.managerVersion}</Tag>
          <Text type="tertiary">{session.user.username || '管理员'}</Text>
        </Space>
      </Header>
      <Content className="app-content">
        {!status.kvmAvailable && (
          <Banner className="section-gap" type="danger" title="未检测到 /dev/kvm" description="请先在 BIOS/UEFI 开启硬件虚拟化，再执行首次安装。" />
        )}
        {!!status.networkCompatibility?.pendingRestart?.length && (
          <Banner
            className="section-gap"
            type="warning"
            title="部分虚拟机需要重启后启用兼容网卡"
            description={`已写入持久网卡配置：${status.networkCompatibility.pendingRestart.join('、')}。管理器不会自动中断正在运行的虚拟机。`}
          />
        )}
        {!!status.networkCompatibility?.errors?.length && (
          <Banner
            className="section-gap"
            type="danger"
            title="飞牛网络兼容检查存在异常"
            description={status.networkCompatibility.errors.join('；')}
          />
        )}
        <Tabs type="line" keepDOM tabPaneMotion={false}>
          <TabPane tab="概览" itemKey="overview">
            <div className="hero-grid section-gap">
              <Card className="hero-card accent-card" shadows="hover">
                <div className="eyebrow">CURRENT STATE</div>
                <Title heading={2}>{status.installed ? status.version || '已安装' : '尚未安装'}</Title>
                <Paragraph type="tertiary">{status.installed ? `${channelName(status.channel, channels)} · 端口 ${status.port}` : '请选择发行渠道并启动图形化安装任务。'}</Paragraph>
                <Space wrap>
                  {status.installed && <Button type="primary" icon={<IconExternalOpen />} onClick={openQVMConsole}>打开 QVMConsole</Button>}
                  <Button icon={<IconRefresh />} loading={loading} onClick={() => void loadStatus()}>刷新状态</Button>
                </Space>
              </Card>
              <div className="metric-grid">
                <Card className="metric-card"><Text type="tertiary">目标服务</Text><div>{stateTag(status.serviceActive, '运行中', '已停止')}</div></Card>
                <Card className="metric-card"><Text type="tertiary">KVM</Text><div>{stateTag(status.kvmAvailable)}</div></Card>
                <Card className="metric-card"><Text type="tertiary">libvirt</Text><div>{stateTag(status.libvirtActive)}</div></Card>
                <Card className="metric-card"><Text type="tertiary">飞牛网络兼容</Text><div>{stateTag(status.networkCompatibility?.enabled, '已启用', '未启用')}</div></Card>
              </div>
            </div>
            <Card title="安装信息" className="section-gap">
              <Descriptions row data={[
                { key: '管理器架构', value: status.architecture },
                { key: '服务自启动', value: status.serviceEnabled ? '已启用' : '未启用' },
                { key: '数据库', value: status.databasePresent ? '可用' : '未创建' },
                { key: '虚拟机网络', value: status.networkCompatibility?.enabled ? `${status.networkCompatibility.network || 'libvirt'} · ${status.networkCompatibility.bridge || '-'}` : '使用上游配置' },
                { key: '最近操作', value: status.lastOperation || '-' },
              ]} />
            </Card>
          </TabPane>

          <TabPane tab="安装与维护" itemKey="maintenance">
            <Card title="选择发行渠道" className="section-gap">
              <div className="channel-grid">
                {channels.map((channel) => (
                  <button
                    type="button"
                    key={channel.id}
                    className={`channel-card ${selectedChannel === channel.id ? 'selected' : ''}`}
                    onClick={() => setSelectedChannel(channel.id)}
                  >
                    <div className="channel-head"><strong>{channel.name}</strong>{status.channel === channel.id && <Tag color="green">当前</Tag>}</div>
                    <p>{channel.description}</p>
                  </button>
                ))}
              </div>
              {!status.installed && (
                <div className="install-options">
                  <Banner
                    type="warning"
                    title="安装前须知"
                    description={(
                      <div className="install-notice">
                        <p>QVMConsole 不调用飞牛官方虚拟机功能，而是独立基于 Linux libvirt 对接，以提供更底层的虚拟机能力。该方案通常可在发行版 Linux 上正常运行，但在飞牛系统上可能出现异常。</p>
                        <p>安装会修改部分系统设置，并安装或调整相关依赖。请先完成重要数据备份，并确认能够接受由此产生的风险后再继续。</p>
                        <p>飞牛内置的 libvirtd 与 QEMU 版本较旧，不在 QVMConsole 的兼容范围内，虚拟机创建、网络、启动和管理等功能可能存在问题。QVMConsole 后续不会在项目源代码上针对此版本做兼容，而是尽量通过应用侧打补丁方式兼容，彻底兼容仍需要飞牛官方升级相关组件。</p>
                      </div>
                    )}
                  />
                  <label><Text strong>服务端口</Text><InputNumber value={installPort} min={1024} max={65535} onChange={(value) => setInstallPort(Number(value || 8080))} /></label>
                  <div className="install-field">
                    <Text strong>用户存储空间</Text>
                    <div className="install-field-control">
                      <div className="storage-select-row">
                        <Select
                          value={selectedStorageDir || undefined}
                          loading={storageOptionsLoading}
                          disabled={storageOptionsLoading || storageOptions.length === 0}
                          optionList={storageOptions.map((option) => ({ label: storageOptionLabel(option), value: option.path }))}
                          emptyContent="未检测到可用存储空间"
                          onChange={(value) => {
                            if (typeof value === 'string') setSelectedStorageDir(value);
                          }}
                        />
                        <Tooltip content="刷新存储空间">
                          <Button
                            className="qvm-act-ic"
                            icon={<IconRefresh />}
                            loading={storageOptionsLoading}
                            disabled={storageOptionsLoading}
                            onClick={() => void loadStorageOptions()}
                          />
                        </Tooltip>
                      </div>
                      <Text type="tertiary" className="storage-option-detail">
                        {selectedStorage
                          ? `${selectedStorage.source || '根文件系统'} · ${selectedStorage.filesystem || '文件系统未知'} · 镜像目录 ${selectedStorage.path}`
                          : '请刷新并选择可用存储空间'}
                      </Text>
                    </div>
                  </div>
                  <Checkbox checked={riskConfirmed} onChange={(event) => setRiskConfirmed(Boolean(event.target.checked))}>
                    我已完成重要数据备份，了解飞牛兼容性限制及系统变更风险，并同意继续安装。
                  </Checkbox>
                  <Checkbox checked={dependencyConfirmed} onChange={(event) => setDependencyConfirmed(Boolean(event.target.checked))}>
                    我已确认管理器可执行 apt update 并安装缺失依赖，但不会执行系统升级。
                  </Checkbox>
                </div>
              )}
              <Space wrap className="action-row">
                {!status.installed ? (
                  <Button type="primary" disabled={!riskConfirmed || !dependencyConfirmed || !status.kvmAvailable || !selectedStorageDir || storageOptionsLoading || busy} loading={submitting} onClick={() => void startJob('install', { channel: selectedChannel, port: installPort, storageDir: selectedStorageDir })}>安装{channelName(selectedChannel, channels)}</Button>
                ) : (
                  <>
                    <Button type="primary" disabled={busy} loading={submitting} onClick={() => void startJob('update', { channel: updateChannel })}>
                      更新到最新版
                    </Button>
                    {!selectedIsCurrent && <Button disabled={busy} loading={submitting} onClick={() => void startJob('switch', { channel: selectedChannel })}>切换到{channelName(selectedChannel, channels)}</Button>}
                  </>
                )}
                <Button disabled={!status.installed || busy} onClick={() => void startJob('repair')}>修复配置</Button>
                <Button disabled={busy} onClick={async () => {
                  try { await request('/api/v1/cache', { method: 'DELETE' }); Toast.success('下载缓存已清理'); }
                  catch (error) { Toast.error(errorMessage(error)); }
                }}>清理缓存</Button>
                <Button type="danger" disabled={!status.installed || busy} onClick={() => setUninstallVisible(true)}>卸载 QVMConsole</Button>
              </Space>
            </Card>
            <Card title="最近任务" className="section-gap">
              <Table
                rowKey="id"
                pagination={false}
                dataSource={jobs}
                columns={[
                  { title: '时间', dataIndex: 'createdAt', render: (value: string) => new Date(value).toLocaleString() },
                  { title: '操作', dataIndex: 'action', render: (value: string) => jobActionLabel(value) },
                  { title: '状态', dataIndex: 'status', render: (value: string) => <Tag color={value === 'succeeded' ? 'green' : value === 'failed' ? 'red' : 'blue'}>{jobStatusLabel(value)}</Tag> },
                  { title: '进度', dataIndex: 'progress', render: (value: number) => <Progress percent={value} size="small" /> },
                  { title: '操作', render: (_: unknown, job: Job) => <Tooltip content="查看任务日志"><Button theme="borderless" className="qvm-act-ic" icon={<IconSetting />} onClick={() => { setActiveJob(job); setJobPanelVisible(true); setJobLog(''); }} /></Tooltip> },
                ]}
              />
            </Card>
          </TabPane>

          <TabPane tab="服务管理" itemKey="service">
            <Card title="服务控制" className="section-gap">
              <Space wrap>
                <Button type="primary" icon={<IconPlay />} disabled={!status.installed || status.serviceActive || busy} loading={submitting} onClick={() => void serviceAction('start')}>启动</Button>
                <Button icon={<IconStop />} disabled={!status.serviceActive || busy} loading={submitting} onClick={() => void serviceAction('stop')}>停止</Button>
                <Button icon={<IconRefresh />} disabled={!status.installed || busy} loading={submitting} onClick={() => void serviceAction('restart')}>重启</Button>
                <Button icon={<IconSetting />} disabled={!status.installed} onClick={() => setPortVisible(true)}>修改端口</Button>
                <Button icon={<IconExternalOpen />} disabled={!status.installed} onClick={openQVMConsole}>新窗口打开</Button>
              </Space>
            </Card>
            <Card title="服务日志" className="section-gap" headerExtraContent={<Space><Button icon={<IconRefresh />} loading={serviceLogLoading} onClick={() => void loadServiceLog()}>刷新</Button><Button onClick={() => window.open(apiPath('/api/v1/logs?lines=2000&download=1'), '_blank')}>下载</Button></Space>}>
              <TextArea className="log-view" value={serviceLog || '点击“刷新”读取最近 500 行日志。'} readonly autosize={{ minRows: 16, maxRows: 28 }} />
            </Card>
          </TabPane>

          <TabPane tab="用户管理" itemKey="users">
            {!status.databasePresent ? (
              <Banner className="section-gap" type="warning" title="用户数据库尚不可用" description="请先安装并启动 QVMConsole，完成数据库初始化后再使用账号管理。" />
            ) : (
              <Card title="QVMConsole 用户" className="section-gap" headerExtraContent={<Space><Button icon={<IconRefresh />} onClick={() => void loadUsers()}>刷新</Button><Button type="danger" onClick={() => setResetVisible(true)}>重置默认管理员</Button></Space>}>
                <Table rowKey="id" columns={userColumns} dataSource={users} pagination={false} />
              </Card>
            )}
          </TabPane>
        </Tabs>
      </Content>

      <SideSheet title="任务实时日志" visible={jobPanelVisible} width={680} onCancel={() => setJobPanelVisible(false)}>
        {activeJob && <>
          <Descriptions row data={[
            { key: '任务', value: jobActionLabel(activeJob.action) },
            { key: '状态', value: jobStatusLabel(activeJob.status) },
            { key: '说明', value: activeJob.error || activeJob.message },
          ]} />
          <Progress className="section-gap" percent={activeJob.progress} stroke={activeJob.status === 'failed' ? '#d94841' : activeJob.status === 'succeeded' ? '#2e9f6b' : '#3d6ef7'} />
          <div ref={jobLogContainerRef}>
            <TextArea className="log-view" value={jobLog || '等待任务输出…'} readonly autosize={{ minRows: 20, maxRows: 32 }} />
          </div>
          <div className="drawer-actions"><Button onClick={() => window.open(apiPath(`/api/v1/jobs/${activeJob.id}/log`), '_blank')}>下载完整日志</Button></div>
        </>}
      </SideSheet>

      <Modal title="卸载 QVMConsole" visible={uninstallVisible} okType="danger" okText="确认卸载" confirmLoading={submitting} onCancel={() => setUninstallVisible(false)} onOk={() => void startJob('uninstall', { purge })} okButtonProps={{ disabled: uninstallConfirmation !== 'UNINSTALL' }}>
        <Banner type="warning" description="默认保留数据库、配置、虚拟机磁盘、模板和用户存储。外部虚拟机资源始终保留。" />
        <Checkbox className="modal-field" checked={purge} onChange={(event) => setPurge(Boolean(event.target.checked))}>同时删除 /opt/kvm-console 中的数据库与配置</Checkbox>
        <Input className="modal-field" value={uninstallConfirmation} onChange={setUninstallConfirmation} placeholder="输入 UNINSTALL 完成二次确认" />
      </Modal>

      <Modal title="重置默认管理员" visible={resetVisible} okType="danger" okText="重置" confirmLoading={submitting} onCancel={() => setResetVisible(false)} onOk={async () => {
        try { setSubmitting(true); const result = await request<{ username: string; password: string }>('/api/v1/users/default-admin/reset', { method: 'POST', body: JSON.stringify({ confirmation: resetConfirmation }) }); Toast.success(`已重置为 ${result.username} / ${result.password}`); setResetVisible(false); await loadUsers(); }
        catch (error) { Toast.error(errorMessage(error)); } finally { setSubmitting(false); }
      }} okButtonProps={{ disabled: resetConfirmation !== 'RESET ADMIN' }}>
        <Banner type="danger" description="此操作会将默认管理员密码重置为 admin123，并清除该账号的邮箱与 TOTP 安全绑定。" />
        <Input className="modal-field" value={resetConfirmation} onChange={setResetConfirmation} placeholder="输入 RESET ADMIN 完成二次确认" />
      </Modal>

      <Modal title={`清除 ${totpUser?.username || ''} 的 TOTP`} visible={Boolean(totpUser)} okType="danger" okText="清除 TOTP" confirmLoading={submitting} onCancel={() => setTotpUser(undefined)} onOk={async () => {
        if (!totpUser) return;
        try { setSubmitting(true); await request(`/api/v1/users/${totpUser.id}/totp/clear`, { method: 'POST', body: JSON.stringify({ confirmation: totpConfirmation }) }); Toast.success('TOTP 已清除'); setTotpUser(undefined); setTotpConfirmation(''); await loadUsers(); }
        catch (error) { Toast.error(errorMessage(error)); } finally { setSubmitting(false); }
      }} okButtonProps={{ disabled: totpConfirmation !== 'CLEAR TOTP' }}>
        <Paragraph>邮箱绑定不会被修改。用户下次登录时无需提供原 TOTP。</Paragraph>
        <Input value={totpConfirmation} onChange={setTotpConfirmation} placeholder="输入 CLEAR TOTP 完成二次确认" />
      </Modal>

      <Modal title={`修改 ${passwordUser?.username || ''} 的管理员密码`} visible={Boolean(passwordUser)} okText="修改密码" confirmLoading={submitting} onCancel={() => setPasswordUser(undefined)} onOk={() => void changePassword()} okButtonProps={{ disabled: password.length < 12 || password !== passwordAgain || passwordConfirmation !== 'CHANGE PASSWORD' }}>
        <Paragraph type="tertiary">至少 12 位，并执行在线泄露密码检测；邮箱与 TOTP 绑定保持不变。</Paragraph>
        <Input className="modal-field" mode="password" value={password} onChange={setPassword} placeholder="新密码（至少 12 位）" />
        <Input className="modal-field" mode="password" value={passwordAgain} onChange={setPasswordAgain} placeholder="再次输入新密码" />
        <Input className="modal-field" value={passwordConfirmation} onChange={setPasswordConfirmation} placeholder="输入 CHANGE PASSWORD 完成二次确认" />
      </Modal>

      <Modal title="修改 QVMConsole 服务端口" visible={portVisible} okText="应用端口" confirmLoading={submitting} onCancel={() => setPortVisible(false)} onOk={async () => {
        try { setSubmitting(true); await request('/api/v1/config/port', { method: 'PUT', body: JSON.stringify({ port: newPort, confirmation: portConfirmation }) }); Toast.success('端口已修改，服务健康检查通过'); setPortVisible(false); await loadStatus(); }
        catch (error) { Toast.error(errorMessage(error)); } finally { setSubmitting(false); }
      }} okButtonProps={{ disabled: portConfirmation !== 'CHANGE PORT' }}>
        <Paragraph type="tertiary">管理器会同步更新 .env、UFW 规则并重启服务。健康检查失败时自动恢复原配置。</Paragraph>
        <InputNumber className="modal-field full-width" value={newPort} min={1024} max={65535} onChange={(value) => setNewPort(Number(value || 8080))} />
        <Input value={portConfirmation} onChange={setPortConfirmation} placeholder="输入 CHANGE PORT 完成二次确认" />
      </Modal>
    </Layout>
  );
}

function jobActionLabel(action: string): string {
  return ({ install: '首次安装', update: '更新', switch: '版本切换', repair: '修复配置', uninstall: '卸载' } as Record<string, string>)[action] || action;
}

function jobStatusLabel(status: string): string {
  return ({ queued: '排队中', running: '执行中', succeeded: '已完成', failed: '失败' } as Record<string, string>)[status] || status;
}
