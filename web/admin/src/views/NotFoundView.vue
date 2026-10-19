<template>
  <el-result icon="warning" title="页面不存在" sub-title="请检查地址是否正确" data-testid="admin-notfound">
    <template #extra><router-link to="/"><el-button type="primary">回到仪表盘</el-button></router-link></template>
  </el-result>
</template>
